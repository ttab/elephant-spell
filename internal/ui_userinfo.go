package internal

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"unicode/utf8"

	"github.com/ttab/howdah"
)

// UserInfo is the howdah component for the /api/me endpoint.
type UserInfo struct {
	auth howdah.Authenticator
	log  *slog.Logger
}

// NewUserInfo creates a new UserInfo component.
func NewUserInfo(logger *slog.Logger, auth howdah.Authenticator) *UserInfo {
	return &UserInfo{
		auth: auth,
		log:  logger,
	}
}

type userInfoResponse struct {
	Name     string `json:"name"`
	Email    string `json:"email"`
	Initials string `json:"initials"`
}

func (u *UserInfo) RegisterRoutes(mux *howdah.PageMux) {
	mux.HandleFunc("GET /api/me", u.apiMe)
}

func (u *UserInfo) apiMe(
	ctx context.Context, w http.ResponseWriter, r *http.Request,
) (*howdah.Page, error) {
	ctx, err := u.auth.RequireAuth(ctx, w, r)
	if err != nil {
		return nil, err
	}

	accessToken, ok := howdah.AccessToken(ctx)
	if !ok {
		return nil, fmt.Errorf("no access token in context")
	}

	var claims struct {
		Name  string `json:"name"`
		Email string `json:"email"`
	}

	err = accessToken.Claims(&claims)
	if err != nil {
		return nil, fmt.Errorf("extract access token claims: %w", err)
	}

	name := claims.Name
	if name == "" && claims.Email != "" {
		parts := strings.SplitN(claims.Email, "@", 2)
		name = parts[0]
	}

	resp := userInfoResponse{
		Name:     name,
		Email:    claims.Email,
		Initials: makeInitials(name),
	}

	w.Header().Set("Content-Type", "application/json")

	err = json.NewEncoder(w).Encode(resp)
	if err != nil {
		u.log.ErrorContext(ctx, "write user info response",
			"err", err)
	}

	return nil, howdah.ErrSkipRender
}

func makeInitials(name string) string {
	parts := strings.Fields(name)
	if len(parts) == 0 {
		return "?"
	}

	var initials strings.Builder

	for _, part := range parts {
		r, _ := utf8.DecodeRuneInString(part)
		if r != utf8.RuneError {
			initials.WriteRune(r)
		}
	}

	result := strings.ToUpper(initials.String())

	runes := []rune(result)
	if len(runes) > 2 {
		return string([]rune{runes[0], runes[len(runes)-1]})
	}

	return result
}
