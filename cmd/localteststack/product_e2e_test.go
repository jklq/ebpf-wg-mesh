package main

import (
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

func TestProductRouteURLRequiresHTTPSForPublicTunnel(t *testing.T) {
	t.Parallel()

	got, err := productRouteURL("coral-abc.mesh.example.test", 8080, "https://mesh.example.test")
	if err != nil {
		t.Fatalf("productRouteURL: %v", err)
	}
	if got != "https://coral-abc.mesh.example.test/" {
		t.Fatalf("unexpected route URL %q", got)
	}

	if _, err := productRouteURL("coral-abc.mesh.example.test", 8080, "http://mesh.example.test"); err == nil {
		t.Fatal("expected http public base URL to be rejected")
	}

	local, err := productRouteURL("coral-abc.platform.localtest.me", 8080, "")
	if err != nil {
		t.Fatalf("local productRouteURL: %v", err)
	}
	if local != "http://coral-abc.platform.localtest.me:8080/" {
		t.Fatalf("unexpected local route URL %q", local)
	}
}

func TestMintDashboardAccessTokenMatchesConsoleClaims(t *testing.T) {
	t.Parallel()

	token, err := mintDashboardAccessToken("test-secret", productE2EUserID, productE2EUserEmail)
	if err != nil {
		t.Fatalf("mintDashboardAccessToken: %v", err)
	}
	parsed, err := jwt.Parse(token, func(token *jwt.Token) (any, error) {
		if token.Method != jwt.SigningMethodHS256 {
			t.Fatalf("unexpected signing method %v", token.Method)
		}
		return []byte("test-secret"), nil
	})
	if err != nil {
		t.Fatalf("parse token: %v", err)
	}
	claims, ok := parsed.Claims.(jwt.MapClaims)
	if !ok || !parsed.Valid {
		t.Fatalf("invalid claims %#v", parsed.Claims)
	}
	if claims["iss"] != dashboardJWTIssuer || claims["aud"] != dashboardJWTAudience {
		t.Fatalf("unexpected iss/aud %#v", claims)
	}
	if claims["typ"] != "access" || claims["sub"] != productE2EUserID || claims["email"] != productE2EUserEmail {
		t.Fatalf("unexpected identity claims %#v", claims)
	}
	exp, ok := claims["exp"].(float64)
	if !ok || time.Unix(int64(exp), 0).Before(time.Now()) {
		t.Fatalf("token missing future exp: %#v", claims["exp"])
	}
	if strings.Count(token, ".") != 2 {
		t.Fatalf("token is not a compact JWT: %q", token)
	}
}
