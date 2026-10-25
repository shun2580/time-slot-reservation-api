// Package config は、環境変数からアプリの設定を読み込む。
package config

import (
	"fmt"
	"log/slog"
	"strings"
)

// 必須の環境変数。既定値は持たない。空文字も「欠け」として扱う。
const (
	envHTTPAddr        = "APP_HTTP_ADDR"
	envDBHost          = "APP_DB_HOST"
	envDBPort          = "APP_DB_PORT"
	envDBUser          = "APP_DB_USER"
	envDBPassword      = "APP_DB_PASSWORD"
	envDBName          = "APP_DB_NAME"
	envKeycloakIssuer  = "APP_KEYCLOAK_ISSUER"
	envKeycloakJWKSURL = "APP_KEYCLOAK_JWKS_URL"
	envJWTAudience     = "APP_JWT_AUDIENCE"
)

// required は欠け検査の順序を固定する(エラー文を毎回同じ並びにするため)。
var required = []string{
	envHTTPAddr,
	envDBHost,
	envDBPort,
	envDBUser,
	envDBPassword,
	envDBName,
	envKeycloakIssuer,
	envKeycloakJWKSURL,
	envJWTAudience,
}

const redacted = "<redacted>"

// Config はアプリの設定である。DBDSN にパスワードが含まれるため、
// fmt や slog に渡しても値が出ないよう String / GoString / LogValue で秘匿する。
type Config struct {
	HTTPAddr        string
	DBDSN           string
	KeycloakIssuer  string
	KeycloakJWKSURL string
	JWTAudience     string
}

// Load は getenv から設定を読む。必須の鍵が欠けていれば、欠けた鍵の名を
// 全て挙げたエラーを返す(値は含めない)。
func Load(getenv func(string) string) (Config, error) {
	values := make(map[string]string, len(required))
	var missing []string
	for _, key := range required {
		v := getenv(key)
		if v == "" {
			missing = append(missing, key)
			continue
		}
		values[key] = v
	}
	if len(missing) > 0 {
		return Config{}, fmt.Errorf("config: missing required environment variables: %s", strings.Join(missing, ", "))
	}

	// parseTime=true と loc=Asia%2FTokyo は、日時を JST の time.Time として扱うための必須の指定。
	dsn := fmt.Sprintf("%s:%s@tcp(%s:%s)/%s?parseTime=true&loc=Asia%%2FTokyo&charset=utf8mb4",
		values[envDBUser], values[envDBPassword], values[envDBHost], values[envDBPort], values[envDBName])

	return Config{
		HTTPAddr:        values[envHTTPAddr],
		DBDSN:           dsn,
		KeycloakIssuer:  values[envKeycloakIssuer],
		KeycloakJWKSURL: values[envKeycloakJWKSURL],
		JWTAudience:     values[envJWTAudience],
	}, nil
}

// String は %v / %+v / %s 用。DBDSN は伏せる。
func (c Config) String() string {
	return fmt.Sprintf("Config{HTTPAddr:%s DBDSN:%s KeycloakIssuer:%s KeycloakJWKSURL:%s JWTAudience:%s}",
		c.HTTPAddr, redacted, c.KeycloakIssuer, c.KeycloakJWKSURL, c.JWTAudience)
}

// GoString は %#v 用。
func (c Config) GoString() string {
	return fmt.Sprintf("config.Config{HTTPAddr:%q, DBDSN:%q, KeycloakIssuer:%q, KeycloakJWKSURL:%q, JWTAudience:%q}",
		c.HTTPAddr, redacted, c.KeycloakIssuer, c.KeycloakJWKSURL, c.JWTAudience)
}

// LogValue は slog 用。DBDSN は伏せる。
func (c Config) LogValue() slog.Value {
	return slog.GroupValue(
		slog.String("http_addr", c.HTTPAddr),
		slog.String("db_dsn", redacted),
		slog.String("keycloak_issuer", c.KeycloakIssuer),
		slog.String("keycloak_jwks_url", c.KeycloakJWKSURL),
		slog.String("jwt_audience", c.JWTAudience),
	)
}
