package middleware

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// allowedAlgs は、受け入れる署名アルゴリズム。ライブラリの鍵選択まかせにせず、ここで固定する。
var allowedAlgs = []string{jwkAlgRS256}

// adminRole は、管理者を表す realm ロールの名前。
const adminRole = "admin"

// authHTTPTimeout は、AuthConfig.HTTPClient を省いたときの、JWKS 取得の待ち時間の上限。
const authHTTPTimeout = jwksFetchTimeout

// 認証に失敗した理由の分類コード。ログにだけ出す。応答には出さない。
const (
	codeMissingHeader  = "missing_authorization"
	codeMalformedHdr   = "malformed_authorization"
	codeMalformedToken = "malformed_token"
	codeBadSignature   = "bad_signature"
	codeExpired        = "token_expired"
	codeNotYetValid    = "token_not_yet_valid"
	codeBadIssuer      = "bad_issuer"
	codeBadAudience    = "bad_audience"
	codeMissingClaim   = "missing_claim"
	codeMissingKID     = "missing_kid"
	codeUnknownKID     = "unknown_kid"
	codeInvalidToken   = "invalid_token"
)

// UserProvisioner は、検証に成功した利用者を users に用意する(初回アクセスなら作る)。
// 実装は usecase 層に置く。この層は repository を直接触らない。
type UserProvisioner interface {
	// EnsureUser は、keycloak の sub に対応する users の id を返す。無ければ作る。
	EnsureUser(ctx context.Context, keycloakSub string) (int64, error)
}

// AuthConfig は、認証ミドルウェアの設定。
type AuthConfig struct {
	// Issuer は、トークンの iss と完全一致させる値(APP_KEYCLOAK_ISSUER)。
	Issuer string
	// JWKSURL は、公開鍵を取る URL(APP_KEYCLOAK_JWKS_URL)。
	JWKSURL string
	// Audience は、トークンの aud が含むべき値(APP_JWT_AUDIENCE)。
	Audience string
	// WriteProblem は、401・500・503 を Problem Details で返す関数。
	// この層は handler 層に依存しない(handler がこの層の Principal を読むため、依存すると循環する)ので、
	// 組み立て側が handler.WriteStatusProblem を渡す。省けない。
	WriteProblem func(w http.ResponseWriter, r *http.Request, status int, detail string)

	// HTTPClient は、JWKS の取得に使う。省くと、待ち時間に上限を付けた既定のものを使う。
	HTTPClient *http.Client
	// Now は、現在時刻。省くと time.Now。exp・nbf の判定と、JWKS の取り直しの間隔に使う。試験用。
	Now func() time.Time
}

// Auth は、Keycloak が発行した JWT(アクセストークン)を検証する認証ミドルウェア。
type Auth struct {
	cfg    AuthConfig
	users  UserProvisioner
	logger *slog.Logger
	jwks   *jwksCache
	parser *jwt.Parser
}

// NewAuth は、認証ミドルウェアを作る。JWKS は、最初に必要になるまで取りに行かない。
// Issuer・JWKSURL・Audience・WriteProblem のどれかが空なら panic する。
// 空の iss は検証を飛ばす意味になってしまい、気づかず通すのを避けるため、起動時に止める。
func NewAuth(cfg AuthConfig, users UserProvisioner, logger *slog.Logger) *Auth {
	if cfg.Issuer == "" || cfg.JWKSURL == "" || cfg.Audience == "" || cfg.WriteProblem == nil || users == nil {
		panic("middleware: NewAuth requires Issuer, JWKSURL, Audience, WriteProblem and a UserProvisioner")
	}
	if cfg.HTTPClient == nil {
		cfg.HTTPClient = &http.Client{Timeout: authHTTPTimeout}
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &Auth{
		cfg:    cfg,
		users:  users,
		logger: logger,
		jwks:   newJWKSCache(cfg.JWKSURL, cfg.HTTPClient, cfg.Now),
		parser: jwt.NewParser(
			jwt.WithValidMethods(allowedAlgs),
			jwt.WithIssuer(cfg.Issuer),
			jwt.WithAudience(cfg.Audience),
			jwt.WithExpirationRequired(),
			jwt.WithTimeFunc(cfg.Now),
		),
	}
}

// Middleware は、Authorization: Bearer のトークンを検証し、成功したら Principal を context に入れて next を呼ぶ。
//
//   - トークンの不備(署名・iss・aud・exp・形式・未知の kid)は 401(WWW-Authenticate: Bearer つき)。
//   - JWKS を取れず検証ができなければ 503。
//   - users を用意できなければ 500。
//
// 失敗の理由は分類コードだけをログに出す。トークン・署名・JWKS の中身は出さない。
func (a *Auth) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		log := a.logger.With("request_id", RequestIDFrom(r.Context()))

		raw, code := bearerToken(r)
		if code != "" {
			a.unauthorized(w, r, log, code)
			return
		}

		claims, err := a.verify(r.Context(), raw)
		if err != nil {
			var unavailable *jwksUnavailableError
			if errors.As(err, &unavailable) {
				log.Error("jwks unavailable", "code", unavailable.code)
				a.cfg.WriteProblem(w, r, http.StatusServiceUnavailable, "")
				return
			}
			a.unauthorized(w, r, log, failureCode(err))
			return
		}

		sub, _ := claims.GetSubject()
		userID, err := a.users.EnsureUser(r.Context(), sub)
		if err != nil {
			// 失敗の中身(DB のエラー文など)は出さない。原因は usecase 側のログで追う。
			log.Error("ensure user failed", "sub", sub)
			a.cfg.WriteProblem(w, r, http.StatusInternalServerError, "")
			return
		}

		p := Principal{Sub: sub, UserID: userID, IsAdmin: hasAdminRole(claims)}
		next.ServeHTTP(w, r.WithContext(withPrincipal(r.Context(), p)))
	})
}

func (a *Auth) unauthorized(w http.ResponseWriter, r *http.Request, log *slog.Logger, code string) {
	log.Info("unauthorized", "code", code)
	w.Header().Set("WWW-Authenticate", "Bearer") // WriteProblem がヘッダを確定させる前に設定する
	a.cfg.WriteProblem(w, r, http.StatusUnauthorized, "")
}

// bearerToken は、Authorization ヘッダから Bearer トークンを取り出す。
// 取り出せなければ、失敗の分類コードを返す。
func bearerToken(r *http.Request) (token, code string) {
	h := r.Header.Get("Authorization")
	if h == "" {
		return "", codeMissingHeader
	}
	scheme, rest, found := strings.Cut(h, " ")
	if !found || !strings.EqualFold(scheme, "Bearer") {
		return "", codeMalformedHdr
	}
	rest = strings.TrimSpace(rest)
	if rest == "" || strings.ContainsAny(rest, " \t") {
		return "", codeMalformedHdr
	}
	return rest, ""
}

// verify は、トークンの署名・iss・aud・exp・nbf を検証し、claims を返す。
// 鍵は kid で引く。署名用(use=sig・alg=RS256)ではない鍵の kid は、未知の kid として扱う。
func (a *Auth) verify(ctx context.Context, raw string) (jwt.MapClaims, error) {
	claims := jwt.MapClaims{}
	_, err := a.parser.ParseWithClaims(raw, claims, func(t *jwt.Token) (any, error) {
		kid, _ := t.Header["kid"].(string)
		if kid == "" {
			return nil, errMissingKID
		}
		return a.jwks.key(ctx, kid)
	})
	if err != nil {
		return nil, err
	}
	if sub, _ := claims.GetSubject(); sub == "" {
		return nil, jwt.ErrTokenRequiredClaimMissing
	}
	return claims, nil
}

// errMissingKID は、トークンのヘッダに kid が無いこと。
var errMissingKID = errors.New("missing kid")

// failureCode は、検証の失敗を分類コードにする。
func failureCode(err error) string {
	switch {
	case errors.Is(err, errMissingKID):
		return codeMissingKID
	case errors.Is(err, errUnknownKID):
		return codeUnknownKID
	case errors.Is(err, jwt.ErrTokenExpired):
		return codeExpired
	case errors.Is(err, jwt.ErrTokenNotValidYet):
		return codeNotYetValid
	case errors.Is(err, jwt.ErrTokenInvalidIssuer):
		return codeBadIssuer
	case errors.Is(err, jwt.ErrTokenInvalidAudience):
		return codeBadAudience
	case errors.Is(err, jwt.ErrTokenRequiredClaimMissing):
		return codeMissingClaim
	case errors.Is(err, jwt.ErrTokenSignatureInvalid):
		return codeBadSignature
	case errors.Is(err, jwt.ErrTokenMalformed):
		return codeMalformedToken
	default:
		return codeInvalidToken
	}
}

// hasAdminRole は、realm_access.roles に "admin" があるかを返す。
// realm_access が無い・空・型が違うときは、管理者ではない(利用者として扱う)。
func hasAdminRole(claims jwt.MapClaims) bool {
	realmAccess, ok := claims["realm_access"].(map[string]any)
	if !ok {
		return false
	}
	roles, ok := realmAccess["roles"].([]any)
	if !ok {
		return false
	}
	for _, role := range roles {
		if s, ok := role.(string); ok && s == adminRole {
			return true
		}
	}
	return false
}
