package payments

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// call sends a request and decodes a JSON response into out (when non-nil and
// the status is 2xx). It returns the status and raw body so adapters can map
// provider errors themselves.
func call(ctx context.Context, hc *http.Client, req *http.Request, out any) (int, []byte, error) {
	req = req.WithContext(ctx)
	req.Header.Set("Accept", "application/json")
	res, err := hc.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return 0, nil, &Error{Code: "provider_unreachable", Message: "the provider didn't answer in time"}
		}
		return 0, nil, &Error{Code: "provider_unreachable", Message: "couldn't reach the provider"}
	}
	defer res.Body.Close()
	body, err := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	if err != nil {
		return res.StatusCode, nil, &Error{Code: "provider_unreachable", Message: "the provider's response was cut off"}
	}
	if out != nil && res.StatusCode/100 == 2 {
		if err := json.Unmarshal(body, out); err != nil {
			return res.StatusCode, body, &Error{Code: "provider_error", Message: "unexpected response from the provider", Status: res.StatusCode}
		}
	}
	return res.StatusCode, body, nil
}

// providerError builds an Error from a non-2xx response, using msg when the
// provider's own message is missing.
func providerError(status int, providerMsg, fallback string) error {
	code := "provider_error"
	if status == http.StatusUnauthorized || status == http.StatusForbidden {
		code = "invalid_credentials"
	}
	m := strings.TrimSpace(providerMsg)
	if m == "" {
		m = fallback
	}
	return &Error{Code: code, Message: truncate(m, 300), Status: status}
}

// IsCode reports whether err is a payments *Error with this code.
func IsCode(err error, code string) bool {
	var e *Error
	return errors.As(err, &e) && e.Code == code
}

func jsonReader(v any) (io.Reader, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("payments: encode request: %w", err)
	}
	return strings.NewReader(string(b)), nil
}
