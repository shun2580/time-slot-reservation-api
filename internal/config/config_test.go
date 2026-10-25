package config

import (
	"bytes"
	"fmt"
	"log/slog"
	"strings"
	"testing"
)

const testPassword = "s3cr3t-P@ss/w0rd"

func fullEnv() map[string]string {
	return map[string]string{
		"APP_HTTP_ADDR":         ":8080",
		"APP_DB_HOST":           "db",
		"APP_DB_PORT":           "3306",
		"APP_DB_USER":           "app",
		"APP_DB_PASSWORD":       testPassword,
		"APP_DB_NAME":           "reservation",
		"APP_KEYCLOAK_ISSUER":   "http://keycloak:8080/realms/test",
		"APP_KEYCLOAK_JWKS_URL": "http://keycloak:8080/realms/test/protocol/openid-connect/certs",
		"APP_JWT_AUDIENCE":      "reservation-api",
	}
}

func getenvFrom(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func TestLoad_AllSet(t *testing.T) {
	cfg, err := Load(getenvFrom(fullEnv()))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.HTTPAddr != ":8080" {
		t.Errorf("HTTPAddr = %q", cfg.HTTPAddr)
	}
	wantDSN := "app:" + testPassword + "@tcp(db:3306)/reservation?parseTime=true&loc=Asia%2FTokyo&charset=utf8mb4"
	if cfg.DBDSN != wantDSN {
		t.Errorf("DBDSN = %q, want %q", cfg.DBDSN, wantDSN)
	}
	if cfg.KeycloakIssuer != "http://keycloak:8080/realms/test" {
		t.Errorf("KeycloakIssuer = %q", cfg.KeycloakIssuer)
	}
	if cfg.KeycloakJWKSURL != "http://keycloak:8080/realms/test/protocol/openid-connect/certs" {
		t.Errorf("KeycloakJWKSURL = %q", cfg.KeycloakJWKSURL)
	}
	if cfg.JWTAudience != "reservation-api" {
		t.Errorf("JWTAudience = %q", cfg.JWTAudience)
	}
}

func TestLoad_DSNHasTimeSettings(t *testing.T) {
	cfg, err := Load(getenvFrom(fullEnv()))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	for _, want := range []string{"parseTime=true", "loc=Asia%2FTokyo"} {
		if !strings.Contains(cfg.DBDSN, want) {
			t.Errorf("DBDSN %q does not contain %q", cfg.DBDSN, want)
		}
	}
}

func TestLoad_MissingOneKey(t *testing.T) {
	for _, key := range required {
		t.Run(key, func(t *testing.T) {
			env := fullEnv()
			delete(env, key)
			_, err := Load(getenvFrom(env))
			if err == nil {
				t.Fatal("want error, got nil")
			}
			if !strings.Contains(err.Error(), key) {
				t.Errorf("error %q does not name %s", err, key)
			}
			assertNoSecret(t, "error", err.Error())
		})
	}
}

func TestLoad_EmptyStringIsMissing(t *testing.T) {
	for _, key := range required {
		t.Run(key, func(t *testing.T) {
			env := fullEnv()
			env[key] = ""
			_, err := Load(getenvFrom(env))
			if err == nil {
				t.Fatal("want error, got nil")
			}
			if !strings.Contains(err.Error(), key) {
				t.Errorf("error %q does not name %s", err, key)
			}
		})
	}
}

func TestLoad_ListsAllMissingKeys(t *testing.T) {
	tests := []struct {
		name    string
		env     map[string]string
		missing []string
	}{
		{"none set", map[string]string{}, required},
		{
			"three missing",
			func() map[string]string {
				e := fullEnv()
				delete(e, "APP_DB_HOST")
				delete(e, "APP_KEYCLOAK_ISSUER")
				e["APP_JWT_AUDIENCE"] = ""
				return e
			}(),
			[]string{"APP_DB_HOST", "APP_KEYCLOAK_ISSUER", "APP_JWT_AUDIENCE"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Load(getenvFrom(tt.env))
			if err == nil {
				t.Fatal("want error, got nil")
			}
			for _, key := range tt.missing {
				if !strings.Contains(err.Error(), key) {
					t.Errorf("error %q does not name %s", err, key)
				}
			}
			assertNoSecret(t, "error", err.Error())
		})
	}
}

func TestLoad_ErrorDoesNotLeakValues(t *testing.T) {
	env := fullEnv()
	delete(env, "APP_DB_NAME")
	_, err := Load(getenvFrom(env))
	if err == nil {
		t.Fatal("want error, got nil")
	}
	assertNoSecret(t, "error", err.Error())
}

func TestConfig_HidesPassword(t *testing.T) {
	cfg, err := Load(getenvFrom(fullEnv()))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	t.Run("fmt", func(t *testing.T) {
		for _, verb := range []string{"%v", "%+v", "%s", "%#v"} {
			assertNoSecret(t, verb, fmt.Sprintf(verb, cfg))
			assertNoSecret(t, verb+" (pointer)", fmt.Sprintf(verb, &cfg))
		}
	})

	t.Run("slog", func(t *testing.T) {
		for name, newHandler := range map[string]func(*bytes.Buffer) slog.Handler{
			"json": func(b *bytes.Buffer) slog.Handler { return slog.NewJSONHandler(b, nil) },
			"text": func(b *bytes.Buffer) slog.Handler { return slog.NewTextHandler(b, nil) },
		} {
			t.Run(name, func(t *testing.T) {
				var buf bytes.Buffer
				logger := slog.New(newHandler(&buf))
				logger.Info("loaded", "config", cfg)
				logger.Info("loaded", slog.Any("config", &cfg))
				assertNoSecret(t, "slog "+name, buf.String())
				if !strings.Contains(buf.String(), ":8080") {
					t.Errorf("slog output lacks non-secret fields: %s", buf.String())
				}
			})
		}
	})
}

func assertNoSecret(t *testing.T, label, got string) {
	t.Helper()
	if strings.Contains(got, testPassword) {
		t.Errorf("%s leaks the password: %s", label, got)
	}
}
