// (C) 2014 Cybozu.  All rights reserved.
// Use of this source code is governed by a BSD-style license
// that can be found in the LICENSE file.

package kintone

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/cookiejar"
	"net/textproto"
	"net/url"
	"os"
	"strings"
	"time"
)

const (
	DEFAULT_TIMEOUT = time.Second * 600 // Default value for App.Timeout
)

// Library internal errors.
var (
	ErrTimeout         = errors.New("Timeout")
	ErrInvalidResponse = errors.New("Invalid Response")
	ErrTooMany         = errors.New("Too many records")
)

// Server-side errors.
type AppError struct {
	HttpStatus     string `json:"-"`       // e.g. "404 NotFound"
	HttpStatusCode int    `json:"-"`       // e.g. 404
	Message        string `json:"message"` // Human readable message.
	Id             string `json:"id"`      // A unique error ID.
	Code           string `json:"code"`    // For machines.
}

func (e *AppError) Error() string {
	if len(e.Message) == 0 {
		return "HTTP error: " + e.HttpStatus
	}
	return fmt.Sprintf("AppError: %d [%s] %s (%s)",
		e.HttpStatusCode, e.Code, e.Message, e.Id)
}

// App provides kintone application API client.
//
// You need to provide Domain, User, Password, and AppId.
// When using Google AppEngine, you must supply Client too.
//
//	import (
//		"appengine"
//		"appengine/urlfetch"
//		"github.com/cybozu/go-kintone"
//		"http"
//	)
//
//	func handler(w http.ResponseWriter, r *http.Request) {
//		c := appengine.NewContext(r)
//		app := &kintone.App{urlfetch.Client(c)}
//		...
//	}
//
// Errors returned by the methods of App may be one of *AppError,
// ErrTimeout, ErrInvalidResponse, or ErrTooMany.
type App struct {
	Domain            string        // domain name.  ex: "bozuman.cybozu.com"
	User              string        // User account for API.
	Password          string        // User password for API.
	AppId             uint64        // application ID.
	Client            *http.Client  // Specialized client.
	Timeout           time.Duration // Timeout for API responses.
	token             string        // auth token.
	basicAuth         bool          // true to use Basic Authentication.
	basicAuthUser     string        // User name for Basic Authentication.
	basicAuthPassword string        // Password for Basic Authentication.
}

// SetBasicAuth enables use of HTTP basic authentication for access
// to kintone.
func (app *App) SetBasicAuth(user, password string) {
	app.basicAuth = true
	app.basicAuthUser = user
	app.basicAuthPassword = password
}

func (app *App) newRequest(method, api string, body io.Reader) (*http.Request, error) {
	if len(app.token) == 0 {
		app.token = base64.StdEncoding.EncodeToString(
			[]byte(app.User + ":" + app.Password))
	}
	u := url.URL{
		Scheme: "https",
		Host:   app.Domain,
		Path:   "/k/v1/" + api + ".json",
	}
	req, err := http.NewRequest(method, u.String(), body)
	if err != nil {
		return nil, err
	}
	if app.basicAuth {
		req.SetBasicAuth(app.basicAuthUser, app.basicAuthPassword)
	}
	req.Header.Set("X-Cybozu-Authorization", app.token)
	req.Header.Set("Content-Type", "application/json")
	return req, nil
}

func (app *App) do(req *http.Request) (*http.Response, error) {
	if app.Client == nil {
		jar, err := cookiejar.New(nil)
		if err != nil {
			return nil, err
		}
		app.Client = &http.Client{Jar: jar}
	}
	if app.Timeout == time.Duration(0) {
		app.Timeout = DEFAULT_TIMEOUT
	}

	type result struct {
		resp *http.Response
		err  error
	}
	done := make(chan result, 1)
	go func() {
		resp, err := app.Client.Do(req)
		done <- result{resp, err}
	}()

	type requestCanceler interface {
		CancelRequest(*http.Request)
	}

	select {
	case r := <-done:
		return r.resp, r.err
	case <-time.After(app.Timeout):
		if canceller, ok := app.Client.Transport.(requestCanceler); ok {
			canceller.CancelRequest(req)
		} else {
			go func() {
				r := <-done
				if r.err == nil {
					r.resp.Body.Close()
				}
			}()
		}
		return nil, ErrTimeout
	}
}

func isJSON(contentType string) bool {
	mediatype, _, err := mime.ParseMediaType(contentType)
	if err != nil {
		return false
	}
	return mediatype == "application/json"
}

func parseResponse(resp *http.Response) ([]byte, error) {
	body, err := ioutil.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		if !isJSON(resp.Header.Get("Content-Type")) {
			return nil, &AppError{
				HttpStatus:     resp.Status,
				HttpStatusCode: resp.StatusCode,
			}
		}
		var ae AppError
		json.Unmarshal(body, &ae)
		ae.HttpStatus = resp.Status
		ae.HttpStatusCode = resp.StatusCode
		return nil, &ae
	}
	return body, nil
}

// GetRecord fetches a record.
func (app *App) GetRecord(id uint64) (Record, error) {
	type request_body struct {
		App uint64 `json:"app,string"`
		Id  uint64 `json:"id,string"`
	}
	data, _ := json.Marshal(request_body{app.AppId, id})
	req, err := app.newRequest("GET", "record", bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	resp, err := app.do(req)
	if err != nil {
		return nil, err
	}
	body, err := parseResponse(resp)
	if err != nil {
		return nil, err
	}
	rec, err := DecodeRecord(body)
	if err != nil {
		return nil, ErrInvalidResponse
	}
	return rec, nil
}

// GetRecords fetches records matching given conditions.
//
// This method can retrieve up to 100 records at once.
// To retrieve more records, you need to call GetRecords with
// increasing "offset" query parameter until the number of records
// retrieved becomes less than 100.
//
// If fields is nil, all fields are retrieved.
// See API specs how to construct query strings.
func (app *App) GetRecords(fields []string, query string) ([]Record, error) {
	type request_body struct {
		App    uint64   `json:"app,string"`
		Fields []string `json:"fields"`
		Query  string   `json:"query"`
	}
	data, _ := json.Marshal(request_body{app.AppId, fields, query})
	req, err := app.newRequest("GET", "records", bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	resp, err := app.do(req)
	if err != nil {
		return nil, err
	}
	body, err := parseResponse(resp)
	if err != nil {
		return nil, err
	}
	recs, err := DecodeRecords(body)
	if err != nil {
		return nil, ErrInvalidResponse
	}
	return recs, nil
}

// GetAllRecords fetches all records.
//
// If fields is nil, all fields are retrieved.
func (app *App) GetAllRecords(fields []string) ([]Record, error) {
	recs := make([]Record, 0, 100)
	type request_body struct {
		App    uint64   `json:"app,string"`
		Fields []string `json:"fields"`
		Query  string   `json:"query"`
	}
	for {
		query := "limit 100"
		if len(recs) > 0 {
			query = fmt.Sprintf("limit 100 offset %v", len(recs))
		}
		data, _ := json.Marshal(request_body{app.AppId, fields, query})
		req, err := app.newRequest("GET", "records", bytes.NewReader(data))
		if err != nil {
			return nil, err
		}
		resp, err := app.do(req)
		if err != nil {
			return nil, err
		}
		body, err := parseResponse(resp)
		if err != nil {
			return nil, err
		}
		r, err := DecodeRecords(body)
		if err != nil {
			return nil, ErrInvalidResponse
		}
		recs = append(recs, r...)
		if len(r) < 100 {
			return recs, nil
		}
	}
}

// FileData stores downloaded file data.
type FileData struct {
	ContentType string    // MIME type of the contents.
	Reader      io.Reader // File contents.
}

// Download fetches an attached file contents.
//
// fileKey should be obtained from FileField (= []File).
func (app *App) Download(fileKey string) (*FileData, error) {
	type request_body struct {
		FileKey string `json:"fileKey"`
	}
	data, _ := json.Marshal(request_body{fileKey})
	req, err := app.newRequest("GET", "file", bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	resp, err := app.do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		if !isJSON(resp.Header.Get("Content-Type")) {
			return nil, &AppError{
				HttpStatus:     resp.Status,
				HttpStatusCode: resp.StatusCode,
			}
		}
		var ae AppError
		body, _ := ioutil.ReadAll(resp.Body)
		resp.Body.Close()
		json.Unmarshal(body, &ae)
		ae.HttpStatus = resp.Status
		ae.HttpStatusCode = resp.StatusCode
		return nil, &ae
	}

	pin, pout := io.Pipe()
	go func() {
		_, err := io.Copy(pout, resp.Body)
		resp.Body.Close()
		if err != nil {
			pout.CloseWithError(err)
		} else {
			pout.Close()
		}
	}()
	return &FileData{resp.Header.Get("Content-Type"), pin}, nil
}

var quoteEscaper = strings.NewReplacer("\\", "\\\\", `"`, "\\\"")

func escapeQuotes(s string) string {
	return quoteEscaper.Replace(s)
}

// Upload uploads a file.
//
// If successfully uploaded, the key string of the uploaded file is returned.
func (app *App) Upload(fileName, contentType string, data io.Reader) (key string, err error) {
	f, err := ioutil.TempFile("", "hoge")
	if err != nil {
		return
	}
	defer func(fn string) {
		_ = os.Remove(fn)
	}(f.Name())

	w := multipart.NewWriter(f)
	h := make(textproto.MIMEHeader)
	h.Set("Content-Disposition",
		fmt.Sprintf(`form-data; name="file"; filename="%s"`,
			escapeQuotes(fileName)))
	h.Set("Content-Type", contentType)
	fw, err := w.CreatePart(h)
	if _, err = io.Copy(fw, data); err != nil {
		return
	}
	if err = w.Close(); err != nil {
		return
	}
	if _, err = f.Seek(0, 0); err != nil {
		return
	}

	req, err := app.newRequest("POST", "file", f)
	if err != nil {
		return
	}
	req.Header.Set("Content-Type", w.FormDataContentType())
	resp, err := app.do(req)
	if err != nil {
		return
	}
	body, err := parseResponse(resp)
	if err != nil {
		return
	}

	var t struct {
		FileKey string `json:"fileKey"`
	}
	if json.Unmarshal(body, &t) != nil {
		err = ErrInvalidResponse
		return
	}
	return t.FileKey, nil
}

// AddRecord adds a new record.
//
// If successful, the record ID of the new record is returned.
func (app *App) AddRecord(rec Record) (id string, err error) {
	type request_body struct {
		App    uint64 `json:"app,string"`
		Record Record `json:"record"`
	}
	data, _ := json.Marshal(request_body{app.AppId, rec})
	req, err := app.newRequest("POST", "record", bytes.NewReader(data))
	if err != nil {
		return
	}
	resp, err := app.do(req)
	if err != nil {
		return
	}
	body, err := parseResponse(resp)
	if err != nil {
		return
	}

	var t struct {
		Id string `json:"id"`
	}
	if json.Unmarshal(body, &t) != nil {
		err = ErrInvalidResponse
		return
	}
	id = t.Id
	return
}

// AddRecords adds new records.
//
// Up to 100 records can be added at once.
// If successful, a list of record IDs is returned.
func (app *App) AddRecords(recs []Record) ([]string, error) {
	if len(recs) > 100 {
		return nil, ErrTooMany
	}

	type request_body struct {
		App     uint64   `json:"app,string"`
		Records []Record `json:"records"`
	}
	data, _ := json.Marshal(request_body{app.AppId, recs})
	req, err := app.newRequest("POST", "records", bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	resp, err := app.do(req)
	if err != nil {
		return nil, err
	}
	body, err := parseResponse(resp)
	if err != nil {
		return nil, err
	}

	var t struct {
		Ids []string `json:"ids"`
	}
	if json.Unmarshal(body, &t) != nil {
		return nil, ErrInvalidResponse
	}
	return t.Ids, nil
}

// UpdateRecord edits a record.
func (app *App) UpdateRecord(id uint64, rec Record) error {
	type request_body struct {
		App    uint64 `json:"app,string"`
		Id     uint64 `json:"id,string"`
		Record Record `json:"record"`
	}
	data, _ := json.Marshal(request_body{app.AppId, id, rec})
	req, err := app.newRequest("PUT", "record", bytes.NewReader(data))
	if err != nil {
		return err
	}
	resp, err := app.do(req)
	if err != nil {
		return err
	}
	_, err = parseResponse(resp)
	return err
}

// UpdateRecords edits multiple records.
//
// "recs" is a mapping between record IDs and Record data.
// Up to 100 records can be edited at once.
func (app *App) UpdateRecords(recs map[uint64]Record) error {
	if len(recs) > 100 {
		return ErrTooMany
	}

	type update_t struct {
		Id     uint64 `json:"id,string"`
		Record Record `json:"record"`
	}
	type request_body struct {
		App     uint64     `json:"app,string"`
		Records []update_t `json:"records"`
	}
	t_recs := make([]update_t, 0, len(recs))
	for id, rec := range recs {
		t_recs = append(t_recs, update_t{id, rec})
	}
	data, _ := json.Marshal(request_body{app.AppId, t_recs})
	req, err := app.newRequest("PUT", "records", bytes.NewReader(data))
	if err != nil {
		return err
	}
	resp, err := app.do(req)
	if err != nil {
		return err
	}
	_, err = parseResponse(resp)
	return err
}

// DeleteRecords deletes multiple records.
//
// Up to 100 records can be deleted at once.
func (app *App) DeleteRecords(ids []uint64) error {
	if len(ids) > 100 {
		return ErrTooMany
	}

	type request_body struct {
		App uint64   `json:"app,string"`
		Ids []uint64 `json:"ids,string"`
	}
	data, _ := json.Marshal(request_body{app.AppId, ids})
	req, err := app.newRequest("DELETE", "records", bytes.NewReader(data))
	if err != nil {
		return err
	}
	resp, err := app.do(req)
	if err != nil {
		return err
	}
	_, err = parseResponse(resp)
	return err
}

// FieldInfo is the meta data structure of a field.
type FieldInfo struct {
	Label       string      `json:"label"`             // Label string
	Code        string      `json:"code"`              // Unique field code
	Type        string      `json:"type"`              // Field type.  One of FT_* constant.
	NoLabel     bool        `json:"noLabel"`           // true to hide the label
	Required    bool        `json:"required"`          // true if this field must be filled
	Unique      bool        `json:"unique"`            // true if field values must be unique
	MaxValue    interface{} `json:"maxValue"`          // nil or numeric string
	MinValue    interface{} `json:"minValue"`          // nil or numeric string
	MaxLength   interface{} `json:"maxLength"`         // nil or numeric string
	MinLength   interface{} `json:"minLength"`         // nil or numeric string
	Default     interface{} `json:"defaultValue"`      // anything
	DefaultTime interface{} `json:"defaultExpression"` // nil or "NOW"
	Options     []string    `json:"options"`           // list of selectable values
	Expression  string      `json:"expression"`        // to calculate values
	Separator   bool        `json:"digit"`             // true to use thousand separator
	Medium      string      `json:"protocol"`          // "WEB", "CALL", or "MAIL"
	Format      string      `json:"format"`            // "NUMBER", "NUMBER_DIGIT", "DATETIME", "DATE", "TIME", "HOUR_MINUTE", "DAY_HOUR_MINUTE"
}

// Work around code to handle "true"/"false" strings as booleans...
func (fi *FieldInfo) UnmarshalJSON(data []byte) error {
	var t struct {
		Label       string      `json:"label"`
		Code        string      `json:"code"`
		Type        string      `json:"type"`
		NoLabel     string      `json:"noLabel"`
		Required    string      `json:"required"`
		Unique      string      `json:"unique"`
		MaxValue    interface{} `json:"maxValue"`
		MinValue    interface{} `json:"minValue"`
		MaxLength   interface{} `json:"maxLength"`
		MinLength   interface{} `json:"minLength"`
		Default     interface{} `json:"defaultValue"`
		DefaultTime interface{} `json:"defaultExpression"`
		Options     []string    `json:"options"`
		Expression  string      `json:"expression"`
		Separator   string      `json:"digit"`
		Medium      string      `json:"protocol"`
		Format      string      `json:"format"`
	}
	err := json.Unmarshal(data, &t)
	if err != nil {
		return err
	}
	*fi = FieldInfo{
		t.Label, t.Code, t.Type,
		(t.NoLabel == "true"),
		(t.Required == "true"),
		(t.Unique == "true"),
		t.MaxValue, t.MinValue, t.MaxLength, t.MinLength,
		t.Default, t.DefaultTime, t.Options, t.Expression,
		(t.Separator == "true"),
		t.Medium, t.Format}
	return nil
}

// Fields returns the meta data of the fields in this application.
//
// If successful, a mapping between field codes and FieldInfo is returned.
func (app *App) Fields() (map[string]*FieldInfo, error) {
	type request_body struct {
		App uint64 `json:"app,string"`
	}
	data, _ := json.Marshal(request_body{app.AppId})
	req, err := app.newRequest("GET", "form", bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	resp, err := app.do(req)
	if err != nil {
		return nil, err
	}
	body, err := parseResponse(resp)
	if err != nil {
		return nil, err
	}

	var t struct {
		Properties []FieldInfo `json:"properties"`
	}
	err = json.Unmarshal(body, &t)
	if err != nil {
		return nil, ErrInvalidResponse
	}

	ret := make(map[string]*FieldInfo)
	for i, _ := range t.Properties {
		fi := &(t.Properties[i])
		ret[fi.Code] = fi
	}
	return ret, nil
}