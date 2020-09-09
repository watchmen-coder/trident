package okta

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"golang.org/x/time/rate"

	"trident/pkg/event"
	"trident/pkg/nozzle"
)

const (
    // See https://bugs.chromium.org/p/chromium/issues/detail?id=955620
	FrozenUserAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64)" +
		"AppleWebKit/537.36 (KHTML, like Gecko) Chrome/75.0.3764.0 Safari/537.36"
)

type OktaDriver struct{}

func init() {
	nozzle.Register("okta", OktaDriver{})
}

func (OktaDriver) New(opts map[string]string) (nozzle.Nozzle, error) {
	domain, ok := opts["domain"]
	if !ok {
		return nil, fmt.Errorf("okta nozzle requires 'domain' config parameter")
	}

	// Rate limit requests from the same worker to a maximum of 5/s
	rl := rate.NewLimiter(rate.Every(300*time.Millisecond), 1)

	return &OktaNozzle{
		Domain:      domain,
		UserAgent:   FrozenUserAgent,
		RateLimiter: rl,
	}, nil
}

type OktaNozzle struct {
	// Domain is the Okta subdomain
	Domain string

	// UserAgent will override the Go-http-client user-agent in requests
	UserAgent string

	// RateLimiter controls how frequently we send requests to Okta
	RateLimiter *rate.Limiter
}

type OktaAuthRequest struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

type OktaAuthResponse struct {
	Status   string                 `json:"status"`
	Factor   string                 `json:"factorResult"`
	Embedded map[string]interface{} `json:"_embedded"`
}

func (n *OktaNozzle) Login(username, password string) (*event.AuthResponse, error) {
	ctx := context.Background()
	err := n.RateLimiter.Wait(ctx)
	if err != nil {
		return nil, err
	}

	url := fmt.Sprintf("https://%s.okta.com/api/v1/authn", n.Domain)
	data, _ := json.Marshal(map[string]string{
		"username": username,
		"password": password,
	})
	req, err := http.NewRequest("POST", url, bytes.NewBuffer(data))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", n.UserAgent)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}

	switch resp.StatusCode {
	case 200:
		var res OktaAuthResponse
		err = json.NewDecoder(resp.Body).Decode(&res)
		if err != nil {
			return nil, err
		}

		return &event.AuthResponse{
			Valid:    res.Status != "LOCKED_OUT",
			MFA:      res.Status == "MFA_REQUIRED",
			Locked:   res.Status == "LOCKED_OUT",
			Metadata: res.Embedded,
		}, nil
	case 401:
		return &event.AuthResponse{
			Valid: false,
		}, nil
	case 429:
		return &event.AuthResponse{
			RateLimited: true,
		}, nil
	}

	return nil, fmt.Errorf("unhandled status code from okta provider: %d", resp.StatusCode)

}
