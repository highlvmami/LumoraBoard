package auth

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"

	"golang.org/x/oauth2"
	"golang.org/x/oauth2/endpoints"
)

// Provider is an OAuth 2 identity provider.
type Provider struct {
	ID   string // path segment: /auth/{ID}/login
	Name string // shown on the sign-in button
	// OAuth carries the client credentials and endpoints. RedirectURL is
	// filled in by the Service from its public URL.
	OAuth oauth2.Config
	// UserInfoURL is fetched with the access token after the exchange.
	UserInfoURL string
	// Parse turns the user info response into a Profile.
	Parse func(body []byte) (Profile, error)
}

// GitHub builds the GitHub provider.
func GitHub(clientID, clientSecret string) *Provider {
	return &Provider{
		ID:   "github",
		Name: "GitHub",
		OAuth: oauth2.Config{
			ClientID:     clientID,
			ClientSecret: clientSecret,
			Endpoint:     endpoints.GitHub,
			Scopes:       []string{"read:user"},
		},
		UserInfoURL: "https://api.github.com/user",
		Parse: func(body []byte) (Profile, error) {
			var u struct {
				ID        int64  `json:"id"`
				Login     string `json:"login"`
				Name      string `json:"name"`
				AvatarURL string `json:"avatar_url"`
			}
			if err := json.Unmarshal(body, &u); err != nil {
				return Profile{}, err
			}
			if u.ID == 0 {
				return Profile{}, fmt.Errorf("github user without id")
			}
			name := u.Name
			if name == "" {
				name = u.Login
			}
			return Profile{Subject: strconv.FormatInt(u.ID, 10), Name: name, Avatar: u.AvatarURL}, nil
		},
	}
}

// Google builds the Google provider (OpenID Connect userinfo).
func Google(clientID, clientSecret string) *Provider {
	return &Provider{
		ID:   "google",
		Name: "Google",
		OAuth: oauth2.Config{
			ClientID:     clientID,
			ClientSecret: clientSecret,
			Endpoint:     endpoints.Google,
			Scopes:       []string{"openid", "profile"},
		},
		UserInfoURL: "https://openidconnect.googleapis.com/v1/userinfo",
		Parse: func(body []byte) (Profile, error) {
			var u struct {
				Sub     string `json:"sub"`
				Name    string `json:"name"`
				Picture string `json:"picture"`
			}
			if err := json.Unmarshal(body, &u); err != nil {
				return Profile{}, err
			}
			if u.Sub == "" {
				return Profile{}, fmt.Errorf("google user without sub")
			}
			return Profile{Subject: u.Sub, Name: u.Name, Avatar: u.Picture}, nil
		},
	}
}

// fetchProfile calls the provider's user info endpoint with the token.
func (p *Provider) fetchProfile(ctx context.Context, client *http.Client, tok *oauth2.Token) (Profile, error) {
	ctx = context.WithValue(ctx, oauth2.HTTPClient, client)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.UserInfoURL, nil)
	if err != nil {
		return Profile{}, fmt.Errorf("user info: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	resp, err := p.OAuth.Client(ctx, tok).Do(req)
	if err != nil {
		return Profile{}, fmt.Errorf("user info: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return Profile{}, fmt.Errorf("user info: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return Profile{}, fmt.Errorf("user info: status %d", resp.StatusCode)
	}
	prof, err := p.Parse(body)
	if err != nil {
		return Profile{}, fmt.Errorf("user info: %w", err)
	}
	if prof.Name == "" {
		prof.Name = p.Name + " user"
	}
	return prof, nil
}
