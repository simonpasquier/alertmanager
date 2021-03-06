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

package opsgenie

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	"net/url"
	"testing"
	"time"

	"github.com/go-kit/kit/log"
	commoncfg "github.com/prometheus/common/config"
	"github.com/prometheus/common/model"
	"github.com/stretchr/testify/require"

	"github.com/prometheus/alertmanager/config"
	"github.com/prometheus/alertmanager/notify"
	"github.com/prometheus/alertmanager/notify/test"
	"github.com/prometheus/alertmanager/types"
)

func TestOpsGenieRetry(t *testing.T) {
	notifier := new(Notifier)

	retryCodes := append(test.DefaultRetryCodes(), http.StatusTooManyRequests)
	for statusCode, expected := range test.RetryTests(retryCodes) {
		actual, _ := notifier.retry(statusCode, nil)
		require.Equal(t, expected, actual, fmt.Sprintf("error on status %d", statusCode))
	}
}

func TestOpsGenieErr(t *testing.T) {
	notifier := new(Notifier)
	for _, tc := range []struct {
		status   int
		body     io.Reader
		expected string
	}{
		{
			status:   http.StatusUnprocessableEntity,
			body:     nil,
			expected: "unexpected status code 422",
		},
		{
			status:   http.StatusUnprocessableEntity,
			body:     bytes.NewBuffer([]byte(`{"message":"Request body is not processable. Please check the errors.","errors":{"priority":"should be one of [ P1, P2, P3, P4, P5 ]"},"took":0.002,"requestId":"865a4f83-99d9-48c8-9550-42a375a3a387"}`)),
			expected: `unexpected status code 422: {"message":"Request body is not processable. Please check the errors.","errors":{"priority":"should be one of [ P1, P2, P3, P4, P5 ]"},"took":0.002,"requestId":"865a4f83-99d9-48c8-9550-42a375a3a387"}`,
		},
		{
			status:   http.StatusInternalServerError,
			body:     bytes.NewBuffer([]byte("internal error")),
			expected: "unexpected status code 500: internal error",
		},
	} {
		t.Run("", func(t *testing.T) {
			_, err := notifier.retry(tc.status, tc.body)
			require.Equal(t, err.Error(), tc.expected)
		})
	}
}

func TestOpsGenieRedactedURL(t *testing.T) {
	ctx, u, fn := test.GetContextWithCancelingURL()
	defer fn()

	key := "key"
	notifier, err := New(
		&config.OpsGenieConfig{
			APIURL:     &config.URL{URL: u},
			APIKey:     config.Secret(key),
			HTTPConfig: &commoncfg.HTTPClientConfig{},
		},
		test.CreateTmpl(t),
		log.NewNopLogger(),
	)
	require.NoError(t, err)

	test.AssertNotifyLeaksNoSecret(t, ctx, notifier, key)
}

func TestOpsGenie(t *testing.T) {
	u, err := url.Parse("https://opsgenie/api")
	if err != nil {
		t.Fatalf("failed to parse URL: %v", err)
	}
	logger := log.NewNopLogger()
	tmpl := test.CreateTmpl(t)
	conf := &config.OpsGenieConfig{
		NotifierConfig: config.NotifierConfig{
			VSendResolved: true,
		},
		Message:     `{{ .CommonLabels.Message }}`,
		Description: `{{ .CommonLabels.Description }}`,
		Source:      `{{ .CommonLabels.Source }}`,
		Responders: []config.OpsGenieConfigResponder{
			{
				Name: `{{ .CommonLabels.ResponderName1 }}`,
				Type: `{{ .CommonLabels.ResponderType1 }}`,
			},
			{
				Name: `{{ .CommonLabels.ResponderName2 }}`,
				Type: `{{ .CommonLabels.ResponderType2 }}`,
			},
		},
		Tags:       `{{ .CommonLabels.Tags }}`,
		Note:       `{{ .CommonLabels.Note }}`,
		Priority:   `{{ .CommonLabels.Priority }}`,
		APIKey:     `{{ .ExternalURL }}`,
		APIURL:     &config.URL{URL: u},
		HTTPConfig: &commoncfg.HTTPClientConfig{},
	}
	notifier, err := New(conf, tmpl, logger)
	require.NoError(t, err)

	ctx := context.Background()
	ctx = notify.WithGroupKey(ctx, "1")

	expectedURL, _ := url.Parse("https://opsgenie/apiv2/alerts")

	// Empty alert.
	alert1 := &types.Alert{
		Alert: model.Alert{
			StartsAt: time.Now(),
			EndsAt:   time.Now().Add(time.Hour),
		},
	}
	expectedBody := `{"alias":"6b86b273ff34fce19d6b804eff5a3f5747ada4eaa22f1d49c01e52ddb7875b4b","message":"","details":{},"source":""}
`
	req, retry, err := notifier.createRequest(ctx, alert1)
	require.NoError(t, err)
	require.Equal(t, true, retry)
	require.Equal(t, expectedURL, req.URL)
	require.Equal(t, "GenieKey http://am", req.Header.Get("Authorization"))
	require.Equal(t, expectedBody, readBody(t, req))

	// Fully defined alert.
	alert2 := &types.Alert{
		Alert: model.Alert{
			Labels: model.LabelSet{
				"Message":        "message",
				"Description":    "description",
				"Source":         "http://prometheus",
				"ResponderName1": "TeamA",
				"ResponderType1": "team",
				"ResponderName2": "EscalationA",
				"ResponderType2": "escalation",
				"Tags":           "tag1,tag2",
				"Note":           "this is a note",
				"Priority":       "P1",
			},
			StartsAt: time.Now(),
			EndsAt:   time.Now().Add(time.Hour),
		},
	}
	expectedBody = `{"alias":"6b86b273ff34fce19d6b804eff5a3f5747ada4eaa22f1d49c01e52ddb7875b4b","message":"message","description":"description","details":{},"source":"http://prometheus","responders":[{"name":"TeamA","type":"team"},{"name":"EscalationA","type":"escalation"}],"tags":["tag1","tag2"],"note":"this is a note","priority":"P1"}
`
	req, retry, err = notifier.createRequest(ctx, alert2)
	require.NoError(t, err)
	require.Equal(t, true, retry)
	require.Equal(t, expectedBody, readBody(t, req))

	// Broken API Key Template.
	conf.APIKey = "{{ kaput "
	_, _, err = notifier.createRequest(ctx, alert2)
	require.Error(t, err)
	require.Equal(t, err.Error(), "templating error: template: :1: function \"kaput\" not defined")
}

func readBody(t *testing.T, r *http.Request) string {
	t.Helper()
	body, err := ioutil.ReadAll(r.Body)
	require.NoError(t, err)
	return string(body)
}
