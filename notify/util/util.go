// Copyright 2019 Prometheus Team
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package util

import (
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	"net/url"

	"github.com/go-kit/kit/log"
	"github.com/go-kit/kit/log/level"

	"github.com/prometheus/alertmanager/notify"
	"github.com/prometheus/alertmanager/template"
	"github.com/prometheus/alertmanager/types"
)

// RedactURL removes the URL part from an error of *url.Error type.
func RedactURL(err error) error {
	e, ok := err.(*url.Error)
	if !ok {
		return err
	}
	e.URL = "<redacted>"
	return e
}

// PostJSON sends a POST request with JSON payload to the given URL.
func PostJSON(ctx context.Context, client *http.Client, url string, body io.Reader) (*http.Response, error) {
	return post(ctx, client, url, "application/json", body)
}

// PostText sends a POST request with text payload to the given URL.
func PostText(ctx context.Context, client *http.Client, url string, body io.Reader) (*http.Response, error) {
	return post(ctx, client, url, "text/plain", body)
}

func post(ctx context.Context, client *http.Client, url string, bodyType string, body io.Reader) (*http.Response, error) {
	req, err := http.NewRequest("POST", url, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", bodyType)
	return client.Do(req.WithContext(ctx))
}

// Drain consumes and closes the response's body to make sure that the
// HTTP client can reuse existing connections.
func Drain(r *http.Response) {
	io.Copy(ioutil.Discard, r.Body)
	r.Body.Close()
}

// Truncate truncates a string to fit the given size.
func Truncate(s string, n int) (string, bool) {
	r := []rune(s)
	if len(r) <= n {
		return s, false
	}
	if n <= 3 {
		return string(r[:n]), true
	}
	return string(r[:n-3]) + "...", true
}

// TmplText is using monadic error handling in order to make string templating
// less verbose. Use with care as the final error checking is easily missed.
func TmplText(tmpl *template.Template, data *template.Data, err *error) func(string) string {
	return func(name string) (s string) {
		if *err != nil {
			return
		}
		s, *err = tmpl.ExecuteTextString(name, data)
		return s
	}
}

// TmplHTML is using monadic error handling in order to make string templating
// less verbose. Use with care as the final error checking is easily missed.
func TmplHTML(tmpl *template.Template, data *template.Data, err *error) func(string) string {
	return func(name string) (s string) {
		if *err != nil {
			return
		}
		s, *err = tmpl.ExecuteHTMLString(name, data)
		return s
	}
}

// GroupKey is a string that can be hashed.
type GroupKey string

// ExtractGroupKey gets the group key from the context.
func ExtractGroupKey(ctx context.Context) (GroupKey, error) {
	key, ok := notify.GroupKey(ctx)
	if !ok {
		return "", fmt.Errorf("group key missing")
	}
	return GroupKey(key), nil
}

// Hash returns the sha256 for a group key as integrations may have
// maximum length requirements on deduplication keys.
func (k GroupKey) Hash() string {
	h := sha256.New()
	// hash.Hash.Write never returns an error.
	//nolint: errcheck
	h.Write([]byte(string(k)))
	return fmt.Sprintf("%x", h.Sum(nil))
}

func (k GroupKey) String() string {
	return string(k)
}

// GetTemplateData creates the template data from the context and the alerts.
func GetTemplateData(ctx context.Context, tmpl *template.Template, alerts []*types.Alert, l log.Logger) *template.Data {
	recv, ok := notify.ReceiverName(ctx)
	if !ok {
		level.Error(l).Log("msg", "Missing receiver")
	}
	groupLabels, ok := notify.GroupLabels(ctx)
	if !ok {
		level.Error(l).Log("msg", "Missing group labels")
	}
	return tmpl.Data(recv, groupLabels, alerts...)
}
