package workflowcheck

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

type client struct {
	baseURL string
	http    *http.Client
}

type response struct {
	status  int
	body    []byte
	headers http.Header
}

func newClient(baseURL string) *client {
	return &client{
		baseURL: strings.TrimRight(baseURL, "/"),
		http: &http.Client{
			Timeout: 5 * time.Second,
		},
	}
}

func (c *client) call(
	method string,
	path string,
	payload any,
	expectedVersion *int64,
	extraHeaders ...http.Header,
) (response, error) {
	var body io.Reader
	if payload != nil {
		encoded, err := json.Marshal(payload)
		if err != nil {
			return response{}, fmt.Errorf("encode request: %w", err)
		}
		body = bytes.NewReader(encoded)
	}
	request, err := http.NewRequest(method, c.baseURL+path, body)
	if err != nil {
		return response{}, fmt.Errorf("create request: %w", err)
	}
	if payload != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	if expectedVersion != nil {
		request.Header.Set("X-Expected-Version", fmt.Sprintf("%d", *expectedVersion))
	}
	for _, headers := range extraHeaders {
		for name, values := range headers {
			for _, value := range values {
				request.Header.Add(name, value)
			}
		}
	}
	raw, err := c.http.Do(request)
	if err != nil {
		return response{}, fmt.Errorf("perform request: %w", err)
	}
	defer raw.Body.Close()
	content, err := io.ReadAll(io.LimitReader(raw.Body, 2<<20))
	if err != nil {
		return response{}, fmt.Errorf("read response: %w", err)
	}
	return response{status: raw.StatusCode, body: content, headers: raw.Header}, nil
}

func terminalHeader(terminal string) http.Header {
	headers := http.Header{}
	headers.Set("X-Cellar-Terminal", terminal)
	return headers
}

func (r response) decode(target any) error {
	if err := json.Unmarshal(r.body, target); err != nil {
		return fmt.Errorf("decode response %q: %w", string(r.body), err)
	}
	return nil
}

func (r response) errorCode() string {
	var body struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(r.body, &body); err != nil {
		return ""
	}
	return body.Error.Code
}

func (r response) retryAfter() (string, error) {
	value := r.headers.Get("Retry-After")
	if value == "" {
		return "", fmt.Errorf("response is missing the Retry-After header: %s", strings.TrimSpace(string(r.body)))
	}
	return value, nil
}

func requireStatus(actual response, expected int, operation string) error {
	if actual.status != expected {
		return fmt.Errorf("%s: expected status %d, got %d: %s",
			operation, expected, actual.status, strings.TrimSpace(string(actual.body)))
	}
	return nil
}

func requireCode(actual response, expectedStatus int, expectedCode, operation string) error {
	if err := requireStatus(actual, expectedStatus, operation); err != nil {
		return err
	}
	if actual.errorCode() != expectedCode {
		return fmt.Errorf("%s: expected error %q, got %q",
			operation, expectedCode, actual.errorCode())
	}
	return nil
}
