package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"
)

func TestGenerateAdminPasswordAndHash(t *testing.T) {
	first, err := generateAdminPassword()
	if err != nil {
		t.Fatal(err)
	}
	second, err := generateAdminPassword()
	if err != nil {
		t.Fatal(err)
	}
	if first == second || len(first) < 24 || strings.ContainsAny(first, " +/=") {
		t.Fatalf("password is not suitable for log/copy use: %q", first)
	}
	hash, err := hashAdminPassword(first)
	if err != nil {
		t.Fatal(err)
	}
	if bcrypt.CompareHashAndPassword([]byte(hash), []byte(first)) != nil {
		t.Fatal("generated password does not match its hash")
	}
	if bcrypt.CompareHashAndPassword([]byte(hash), []byte(second)) == nil {
		t.Fatal("different password matched hash")
	}
}

func TestAdminSessionCookieSecurityFollowsRequestProtocol(t *testing.T) {
	expires := time.Now().Add(time.Hour)
	httpRequest := httptest.NewRequest(http.MethodGet, "http://proxy/admin/login", nil)
	httpResponse := httptest.NewRecorder()
	setAdminSessionCookie(httpResponse, httpRequest, "http-token", expires)
	httpCookie := httpResponse.Result().Cookies()[0]
	if httpCookie.Secure || !httpCookie.HttpOnly || httpCookie.SameSite != http.SameSiteLaxMode {
		t.Fatalf("unexpected HTTP cookie: %#v", httpCookie)
	}

	httpsRequest := httptest.NewRequest(http.MethodGet, "https://proxy/admin/login", nil)
	httpsRequest.Header.Set("X-Forwarded-Proto", "https")
	httpsResponse := httptest.NewRecorder()
	setAdminSessionCookie(httpsResponse, httpsRequest, "https-token", expires)
	httpsCookie := httpsResponse.Result().Cookies()[0]
	if !httpsCookie.Secure {
		t.Fatalf("HTTPS cookie is not Secure: %#v", httpsCookie)
	}
}

func TestAdminRoutesRequireAuthentication(t *testing.T) {
	proxy := &ProxyServer{cfg: Config{}, store: nil}

	login := httptest.NewRecorder()
	proxy.handleAdmin(login, httptest.NewRequest(http.MethodGet, "/admin/login", nil))
	if login.Code != http.StatusOK || !strings.Contains(login.Body.String(), "登录") {
		t.Fatalf("login page unavailable: status=%d body=%s", login.Code, login.Body.String())
	}

	ui := httptest.NewRecorder()
	proxy.handleAdmin(ui, httptest.NewRequest(http.MethodGet, "/admin/ui", nil))
	if ui.Code != http.StatusFound || !strings.HasPrefix(ui.Header().Get("Location"), "/admin/login?") {
		t.Fatalf("unauthenticated UI was not redirected: status=%d location=%q", ui.Code, ui.Header().Get("Location"))
	}

	api := httptest.NewRecorder()
	proxy.handleAdmin(api, httptest.NewRequest(http.MethodGet, "/admin/records", nil))
	if api.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated API status=%d body=%s", api.Code, api.Body.String())
	}
}

func TestRedirectToLoginRejectsExternalNext(t *testing.T) {
	response := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/admin/ui", nil)
	redirectToLogin(response, request, "https://attacker.example/steal")
	if got := response.Header().Get("Location"); got != "/admin/login?next=%2Fadmin%2Fui" {
		t.Fatalf("unsafe redirect target: %q", got)
	}
}

func TestBearerToken(t *testing.T) {
	headers := make(http.Header)
	headers.Set("Authorization", "Bearer example")
	if got := bearerToken(headers); got != "example" {
		t.Fatalf("unexpected bearer token: %q", got)
	}
	headers.Del("Authorization")
	headers.Set("X-Prompt-Audit-Admin-Token", "legacy")
	if got := bearerToken(headers); got != "legacy" {
		t.Fatalf("unexpected legacy token: %q", got)
	}
}
