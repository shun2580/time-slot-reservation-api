package middleware_test

// 認証ミドルウェアの試験。鍵は試験内で生成し、JWKS は httptest で立てる(Keycloak も docker も使わない)。
// この層は internal/handler を import しない(handler がこの層の Principal を読むため循環する)。
// エラー応答の書出関数は差込口(ErrorWriter)から渡すので、試験では handler.WriteStatusProblem と同じ形を返す
// fake を使い、401・500・503 の status・ヘッダ・本文の形を確かめる。

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"go/parser"
	"go/token"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/shun2580/time-slot-reservation-api/internal/middleware"
)

// contentTypeProblem は、Problem Details の Content-Type(handler 層の定数と同じ値)。
const contentTypeProblem = "application/problem+json"

const (
	testIssuer   = "http://localhost:8081/realms/slotres"
	testAudience = "slot-api"
	testKID      = "sig-1"
	testSub      = "11111111-2222-3333-4444-555555555555"
	testUserID   = int64(42)
)

var (
	testStart = time.Date(2026, 10, 25, 12, 0, 0, 0, time.UTC)

	keyOnce sync.Once
	keyA    *rsa.PrivateKey // 正規の署名鍵
	keyB    *rsa.PrivateKey // 別の鍵(署名不正・新しい鍵・enc 鍵に使う)
)

// testKeys は、RSA 2048 の鍵を2本、初回だけ生成する(試験ごとに作ると遅いため)。
func testKeys(t *testing.T) (a, b *rsa.PrivateKey) {
	t.Helper()
	keyOnce.Do(func() {
		var err error
		if keyA, err = rsa.GenerateKey(rand.Reader, 2048); err != nil {
			panic(err)
		}
		if keyB, err = rsa.GenerateKey(rand.Reader, 2048); err != nil {
			panic(err)
		}
	})
	return keyA, keyB
}

// ---- エラー書出の fake ----

// writeProblem は、handler.WriteStatusProblem と同じ形(type=about:blank・title=HTTP の理由句・
// status・instance)の Problem Details を書く fake。
func writeProblem(w http.ResponseWriter, r *http.Request, status int, detail string) {
	body, _ := json.Marshal(map[string]any{
		"type":     "about:blank",
		"title":    http.StatusText(status),
		"status":   status,
		"detail":   detail,
		"instance": r.URL.Path,
	})
	w.Header().Set("Content-Type", contentTypeProblem)
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

// ---- 時計 ----

type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// ---- JWKS サーバー ----

type jwksServer struct {
	srv  *httptest.Server
	hits atomic.Int32

	mu     sync.Mutex
	keys   []map[string]any
	status int    // 0 なら 200
	body   string // 空でなければ、keys の代わりにこれを返す
}

func newJWKSServer(t *testing.T) *jwksServer {
	t.Helper()
	s := &jwksServer{}
	s.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.hits.Add(1)
		s.mu.Lock()
		defer s.mu.Unlock()
		if s.status != 0 {
			http.Error(w, "boom", s.status)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if s.body != "" {
			_, _ = w.Write([]byte(s.body))
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"keys": s.keys})
	}))
	t.Cleanup(s.srv.Close)
	return s
}

func (s *jwksServer) setKeys(keys ...map[string]any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.keys = keys
}

func (s *jwksServer) addKey(k map[string]any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.keys = append(s.keys, k)
}

func (s *jwksServer) setStatus(status int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.status = status
}

func (s *jwksServer) setBody(body string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.body = body
}

func (s *jwksServer) Hits() int { return int(s.hits.Load()) }

// jwkOf は、公開鍵を JWKS の1エントリ(RFC 7517)にする。use や alg が空なら、その項目を付けない。
func jwkOf(pub *rsa.PublicKey, kid, use, alg string) map[string]any {
	m := map[string]any{
		"kty": "RSA",
		"kid": kid,
		"n":   base64.RawURLEncoding.EncodeToString(pub.N.Bytes()),
		"e":   base64.RawURLEncoding.EncodeToString(bigEndian(pub.E)),
	}
	if use != "" {
		m["use"] = use
	}
	if alg != "" {
		m["alg"] = alg
	}
	return m
}

func bigEndian(e int) []byte {
	var b []byte
	for ; e > 0; e >>= 8 {
		b = append([]byte{byte(e & 0xff)}, b...)
	}
	return b
}

// ---- 利用者の用意(fake) ----

type fakeUsers struct {
	mu   sync.Mutex
	subs []string
	err  error
}

func (f *fakeUsers) EnsureUser(_ context.Context, sub string) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.subs = append(f.subs, sub)
	if f.err != nil {
		return 0, f.err
	}
	return testUserID, nil
}

func (f *fakeUsers) Subs() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.subs...)
}

// ---- ログの捕捉 ----

type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// ---- 試験の環境 ----

type testEnv struct {
	t      *testing.T
	jwks   *jwksServer
	clock  *fakeClock
	users  *fakeUsers
	logs   *syncBuffer
	auth   *middleware.Auth
	key    *rsa.PrivateKey
	other  *rsa.PrivateKey
	nextN  atomic.Int32
	gotP   atomic.Pointer[middleware.Principal]
	handle http.Handler
}

// newEnv は、sig-1(use=sig・alg=RS256)だけを持つ JWKS と、それを使う認証ミドルウェアを作る。
func newEnv(t *testing.T) *testEnv {
	t.Helper()
	a, b := testKeys(t)
	e := &testEnv{
		t:     t,
		jwks:  newJWKSServer(t),
		clock: &fakeClock{t: testStart},
		users: &fakeUsers{},
		logs:  &syncBuffer{},
		key:   a,
		other: b,
	}
	e.jwks.setKeys(jwkOf(&a.PublicKey, testKID, "sig", "RS256"))
	e.build()
	return e
}

// build は、認証ミドルウェアを(JWKS の URL などを変えずに)作り直す。
func (e *testEnv) build() {
	e.auth = middleware.NewAuth(middleware.AuthConfig{
		Issuer:     testIssuer,
		JWKSURL:    e.jwks.srv.URL,
		Audience:   testAudience,
		HTTPClient: e.jwks.srv.Client(),
		Now:        e.clock.Now,
	}, e.users, writeProblem, slog.New(slog.NewJSONHandler(e.logs, nil)))

	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		e.nextN.Add(1)
		if p, ok := middleware.PrincipalFrom(r.Context()); ok {
			e.gotP.Store(&p)
		}
		w.WriteHeader(http.StatusNoContent)
	})
	e.handle = middleware.RequestID(e.auth.Middleware(next))
}

func (e *testEnv) claims(mods ...func(jwt.MapClaims)) jwt.MapClaims {
	c := jwt.MapClaims{
		"iss": testIssuer,
		"aud": testAudience,
		"sub": testSub,
		"iat": e.clock.Now().Unix(),
		"exp": e.clock.Now().Add(5 * time.Minute).Unix(),
	}
	for _, m := range mods {
		m(c)
	}
	return c
}

func without(keys ...string) func(jwt.MapClaims) {
	return func(c jwt.MapClaims) {
		for _, k := range keys {
			delete(c, k)
		}
	}
}

func set(k string, v any) func(jwt.MapClaims) {
	return func(c jwt.MapClaims) { c[k] = v }
}

// sign は、method と key でトークンに署名する。kid が空なら、ヘッダに kid を付けない。
func sign(t *testing.T, method jwt.SigningMethod, key any, kid string, claims jwt.MapClaims) string {
	t.Helper()
	tok := jwt.NewWithClaims(method, claims)
	if kid != "" {
		tok.Header["kid"] = kid
	}
	s, err := tok.SignedString(key)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	return s
}

// valid は、正規の鍵(sig-1)で署名した、通るはずのトークンを返す。
func (e *testEnv) valid(mods ...func(jwt.MapClaims)) string {
	return sign(e.t, jwt.SigningMethodRS256, e.key, testKID, e.claims(mods...))
}

// call は、Authorization ヘッダ(空なら付けない)つきで1リクエスト流す。
func (e *testEnv) call(authz string) *httptest.ResponseRecorder {
	e.gotP.Store(nil)
	req := httptest.NewRequest(http.MethodGet, "/reservations", nil)
	if authz != "" {
		req.Header.Set("Authorization", authz)
	}
	rec := httptest.NewRecorder()
	e.handle.ServeHTTP(rec, req)
	return rec
}

// reset は、これまでの呼び出しの記録(next の呼出回数・Principal・EnsureUser の引数)を消す。
func (e *testEnv) reset() {
	e.nextN.Store(0)
	e.gotP.Store(nil)
	e.users.mu.Lock()
	e.users.subs = nil
	e.users.mu.Unlock()
}

func (e *testEnv) callToken(token string) *httptest.ResponseRecorder {
	return e.call("Bearer " + token)
}

// ---- 検証の補助 ----

type problemBody struct {
	Type   string `json:"type"`
	Title  string `json:"title"`
	Status int    `json:"status"`
	Detail string `json:"detail"`
}

func decodeProblem(t *testing.T, rec *httptest.ResponseRecorder) problemBody {
	t.Helper()
	if ct := rec.Header().Get("Content-Type"); ct != contentTypeProblem {
		t.Fatalf("Content-Type = %q, want %q", ct, contentTypeProblem)
	}
	var p problemBody
	if err := json.Unmarshal(rec.Body.Bytes(), &p); err != nil {
		t.Fatalf("body is not JSON: %v: %s", err, rec.Body.String())
	}
	return p
}

// requireUnauthorized は、401・WWW-Authenticate: Bearer・Problem Details(about:blank・Unauthorized)を確かめる。
func (e *testEnv) requireUnauthorized(rec *httptest.ResponseRecorder) {
	e.t.Helper()
	if rec.Code != http.StatusUnauthorized {
		e.t.Fatalf("status = %d, want 401; body=%s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("WWW-Authenticate"); got != "Bearer" {
		e.t.Errorf("WWW-Authenticate = %q, want %q", got, "Bearer")
	}
	p := decodeProblem(e.t, rec)
	if p.Type != "about:blank" || p.Title != "Unauthorized" || p.Status != 401 {
		e.t.Errorf("problem = %+v, want about:blank / Unauthorized / 401", p)
	}
	if n := e.nextN.Load(); n != 0 {
		e.t.Errorf("next handler was called %d times on a rejected request", n)
	}
	if e.gotP.Load() != nil {
		e.t.Error("a Principal reached the next handler on a rejected request")
	}
	if subs := e.users.Subs(); len(subs) != 0 {
		e.t.Errorf("EnsureUser was called on a rejected request: %v", subs)
	}
}

func (e *testEnv) requireOK(rec *httptest.ResponseRecorder) middleware.Principal {
	e.t.Helper()
	if rec.Code != http.StatusNoContent {
		e.t.Fatalf("status = %d, want 204 (passed through); body=%s", rec.Code, rec.Body.String())
	}
	p := e.gotP.Load()
	if p == nil {
		e.t.Fatal("no Principal in the context of the next handler")
	}
	return *p
}

// ---- 正常系 ----

func TestAuth_ValidUser(t *testing.T) {
	e := newEnv(t)
	rec := e.callToken(e.valid(set("realm_access", map[string]any{"roles": []string{"offline_access"}})))
	p := e.requireOK(rec)
	if p != (middleware.Principal{Sub: testSub, UserID: testUserID, IsAdmin: false}) {
		t.Errorf("principal = %+v", p)
	}
	if rec.Header().Get("WWW-Authenticate") != "" {
		t.Error("WWW-Authenticate must not be set on success")
	}
}

func TestAuth_ValidAdmin(t *testing.T) {
	e := newEnv(t)
	rec := e.callToken(e.valid(set("realm_access", map[string]any{"roles": []string{"offline_access", "admin"}})))
	p := e.requireOK(rec)
	if p != (middleware.Principal{Sub: testSub, UserID: testUserID, IsAdmin: true}) {
		t.Errorf("principal = %+v", p)
	}
	// 管理者でも、sub を渡して UserID を得る。
	if subs := e.users.Subs(); len(subs) != 1 || subs[0] != testSub {
		t.Errorf("EnsureUser subs = %v, want [%s]", subs, testSub)
	}
}

func TestAuth_AudienceAsArray(t *testing.T) {
	e := newEnv(t)
	e.requireOK(e.callToken(e.valid(set("aud", []string{"account", testAudience}))))
}

func TestAuth_BearerSchemeIsCaseInsensitive(t *testing.T) {
	e := newEnv(t)
	e.requireOK(e.call("bearer " + e.valid()))
}

// ロールの有無: realm_access が無い・空・形が違うときは、認証は通り(401 にせず)、利用者として扱う。
func TestAuth_RolesAbsentOrEmpty_AuthenticatedAsUser(t *testing.T) {
	cases := map[string][]func(jwt.MapClaims){
		"realm_access なし":              nil,
		"realm_access が空のオブジェクト":       {set("realm_access", map[string]any{})},
		"roles が空の配列":                  {set("realm_access", map[string]any{"roles": []string{}})},
		"roles に admin 無し":             {set("realm_access", map[string]any{"roles": []string{"user"}})},
		"realm_access が文字列(形が違う)":      {set("realm_access", "admin")},
		"roles が文字列(形が違う)":             {set("realm_access", map[string]any{"roles": "admin"})},
		"roles の要素が admin ではなく入れ子":     {set("realm_access", map[string]any{"roles": []any{[]string{"admin"}}})},
		"resource_access の admin は見ない": {set("resource_access", map[string]any{"slot-api": map[string]any{"roles": []string{"admin"}}})},
		"ロール名の大文字小文字は区別する":             {set("realm_access", map[string]any{"roles": []string{"Admin", "ADMIN"}})},
	}
	for name, mods := range cases {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t)
			p := e.requireOK(e.callToken(e.valid(mods...)))
			if p.IsAdmin {
				t.Errorf("IsAdmin = true, want false")
			}
			if p.Sub != testSub || p.UserID != testUserID {
				t.Errorf("principal = %+v", p)
			}
		})
	}
}

// ---- トークンの不備 → 401 ----

func TestAuth_RejectedTokens(t *testing.T) {
	cases := []struct {
		name  string
		token func(e *testEnv) string
	}{
		{"期限切れ", func(e *testEnv) string {
			return e.valid(set("exp", e.clock.Now().Add(-time.Second).Unix()))
		}},
		{"exp がちょうど現在(期限切れ側)", func(e *testEnv) string {
			return e.valid(set("exp", e.clock.Now().Unix()))
		}},
		{"exp が無い", func(e *testEnv) string { return e.valid(without("exp")) }},
		{"nbf が未来", func(e *testEnv) string {
			return e.valid(set("nbf", e.clock.Now().Add(time.Minute).Unix()))
		}},
		{"署名不正(別の鍵で署名・kid は正規)", func(e *testEnv) string {
			return sign(e.t, jwt.SigningMethodRS256, e.other, testKID, e.claims())
		}},
		{"payload を改竄", func(e *testEnv) string {
			parts := strings.Split(e.valid(), ".")
			forged, _ := json.Marshal(e.claims(set("sub", "attacker")))
			parts[1] = base64.RawURLEncoding.EncodeToString(forged)
			return strings.Join(parts, ".")
		}},
		{"aud 不一致", func(e *testEnv) string { return e.valid(set("aud", "account")) }},
		{"aud 不一致(配列)", func(e *testEnv) string { return e.valid(set("aud", []string{"account", "other"})) }},
		{"aud 無し", func(e *testEnv) string { return e.valid(without("aud")) }},
		{"iss 不一致", func(e *testEnv) string { return e.valid(set("iss", "http://evil.example/realms/slotres")) }},
		{"iss は完全一致(末尾スラッシュ違い)", func(e *testEnv) string { return e.valid(set("iss", testIssuer+"/")) }},
		{"iss 無し", func(e *testEnv) string { return e.valid(without("iss")) }},
		{"sub 無し", func(e *testEnv) string { return e.valid(without("sub")) }},
		{"sub が空", func(e *testEnv) string { return e.valid(set("sub", "")) }},
		{"kid 無し", func(e *testEnv) string {
			return sign(e.t, jwt.SigningMethodRS256, e.key, "", e.claims())
		}},
		{"JWT ではない文字列", func(e *testEnv) string { return "not-a-jwt" }},
		{"3 区切りだが中身が壊れている", func(e *testEnv) string { return "a.b.c" }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			e.requireUnauthorized(e.callToken(tc.token(e)))
		})
	}
}

// ---- alg の固定 ----

func TestAuth_AlgorithmAllowlist(t *testing.T) {
	a, _ := testKeys(t)
	der, err := x509.MarshalPKIXPublicKey(&a.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der})
	pkcs1 := x509.MarshalPKCS1PublicKey(&a.PublicKey)

	cases := []struct {
		name  string
		token func(e *testEnv) string
	}{
		// alg 混同攻撃: 公開鍵のバイト列を HMAC の秘密として署名する。
		{"HS256(公開鍵 DER を秘密に使う)", func(e *testEnv) string {
			return sign(e.t, jwt.SigningMethodHS256, der, testKID, e.claims())
		}},
		{"HS256(公開鍵 PEM を秘密に使う)", func(e *testEnv) string {
			return sign(e.t, jwt.SigningMethodHS256, pemBytes, testKID, e.claims())
		}},
		{"HS256(公開鍵 PKCS1 を秘密に使う)", func(e *testEnv) string {
			return sign(e.t, jwt.SigningMethodHS256, pkcs1, testKID, e.claims())
		}},
		{"HS384", func(e *testEnv) string {
			return sign(e.t, jwt.SigningMethodHS384, der, testKID, e.claims())
		}},
		{"alg none", func(e *testEnv) string {
			return sign(e.t, jwt.SigningMethodNone, jwt.UnsafeAllowNoneSignatureType, testKID, e.claims())
		}},
		// 正規の秘密鍵で署名しても、RS256 以外の alg は通さない(許可リストで固定している証拠)。
		{"RS384(正規の鍵で署名)", func(e *testEnv) string {
			return sign(e.t, jwt.SigningMethodRS384, e.key, testKID, e.claims())
		}},
		{"RS512(正規の鍵で署名)", func(e *testEnv) string {
			return sign(e.t, jwt.SigningMethodRS512, e.key, testKID, e.claims())
		}},
		{"PS256(正規の鍵で署名)", func(e *testEnv) string {
			return sign(e.t, jwt.SigningMethodPS256, e.key, testKID, e.claims())
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			e.requireUnauthorized(e.callToken(tc.token(e)))
		})
	}
}

// ---- 鍵の選び方: use=sig かつ alg=RS256 の鍵だけ(殿の指示) ----

// enc 鍵(use=enc・alg=RSA-OAEP)の kid を指すトークンは、その鍵の秘密鍵で RS256 署名されていても 401。
func TestAuth_EncKeyIsRejected(t *testing.T) {
	a, b := testKeys(t)
	cases := []struct {
		name string
		priv *rsa.PrivateKey // enc 鍵として載せる公開鍵の持ち主
	}{
		{"別の RSA 鍵の enc エントリ", b},
		{"署名鍵と同じ RSA 鍵の enc エントリ(kid だけ違う)", a},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			e.jwks.addKey(jwkOf(&tc.priv.PublicKey, "enc-1", "enc", "RSA-OAEP"))

			// 対照: 同じ JWKS の sig 鍵では通る。
			e.requireOK(e.callToken(e.valid()))
			e.reset()

			// enc 鍵の kid で、その秘密鍵を使って RS256 署名。
			// 取り直しの間隔が過ぎても結果は変わらない(取り直しても署名用の鍵にならない)。
			for i := 0; i < 2; i++ {
				token := sign(t, jwt.SigningMethodRS256, tc.priv, "enc-1", e.claims())
				e.requireUnauthorized(e.callToken(token))
				e.clock.Advance(11 * time.Second)
			}
		})
	}
}

// 署名用の鍵の条件(kty=RSA・use=sig・alg=RS256)を一つでも欠く JWK は、署名が合っても通さない。
func TestAuth_JWKMustBeSigRS256(t *testing.T) {
	_, b := testKeys(t)
	cases := []struct {
		name     string
		use, alg string
	}{
		{"use=enc alg=RSA-OAEP", "enc", "RSA-OAEP"},
		{"use=enc alg=RS256", "enc", "RS256"},
		{"use=sig alg=RSA-OAEP", "sig", "RSA-OAEP"},
		{"use=sig alg=RS384", "sig", "RS384"},
		{"use=sig alg=PS256", "sig", "PS256"},
		{"use 無し alg=RS256", "", "RS256"},
		{"use=sig alg 無し", "sig", ""},
		{"use も alg も無し", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			e.jwks.addKey(jwkOf(&b.PublicKey, "weird-1", tc.use, tc.alg))
			token := sign(t, jwt.SigningMethodRS256, b, "weird-1", e.claims())
			e.requireUnauthorized(e.callToken(token))
		})
	}
}

func TestAuth_JWKFlawsAreSkippedButOthersStillWork(t *testing.T) {
	a, _ := testKeys(t)
	e := newEnv(t)
	e.jwks.setKeys(
		map[string]any{"kty": "RSA", "kid": "broken-n", "use": "sig", "alg": "RS256", "n": "!!!", "e": "AQAB"},
		map[string]any{"kty": "EC", "kid": "ec-1", "use": "sig", "alg": "ES256"},
		map[string]any{"kty": "RSA", "use": "sig", "alg": "RS256", "n": "AQAB", "e": "AQAB"}, // kid 無し
		map[string]any{"kty": "RSA", "kid": "tiny", "use": "sig", "alg": "RS256", "n": base64.RawURLEncoding.EncodeToString([]byte{0xC3, 0x51}), "e": "AQAB"},
		jwkOf(&a.PublicKey, testKID, "sig", "RS256"),
	)
	e.requireOK(e.callToken(e.valid()))
}

func TestAuth_WeakRSAKeyIsNotAccepted(t *testing.T) {
	weak, err := rsa.GenerateKey(rand.Reader, 1024)
	if err != nil {
		t.Fatal(err)
	}
	e := newEnv(t)
	e.jwks.addKey(jwkOf(&weak.PublicKey, "weak-1", "sig", "RS256"))
	e.requireUnauthorized(e.callToken(sign(t, jwt.SigningMethodRS256, weak, "weak-1", e.claims())))
}

// ---- Authorization ヘッダの形 ----

func TestAuth_BadAuthorizationHeader(t *testing.T) {
	e0 := newEnv(t)
	valid := e0.valid()
	cases := map[string]string{
		"ヘッダ無し":                    "",
		"Basic":                    "Basic dXNlcjpwYXNz",
		"スキームのみ(Bearer)":           "Bearer",
		"空のトークン(Bearer と空白)":       "Bearer ",
		"空のトークン(空白が複数)":            "Bearer    ",
		"トークンが2つ":                  "Bearer " + valid + " extra",
		"スキームなしでトークンだけ":            valid,
		"Bearer ではない別のスキーム":        "Token " + valid,
		"Bearer の後ろが壊れた文字列":        "Bearer !!!",
		"Bearer と JWT の間が空白でなくコロン": "Bearer:" + valid,
	}
	for name, h := range cases {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t)
			e.requireUnauthorized(e.call(h))
		})
	}
}

// ---- JWKS の取得とキャッシュ ----

// 起動時(NewAuth)には JWKS を取りに行かない。最初に必要になったときに取り、以後はキャッシュを使う。
func TestJWKS_LazyAndCached(t *testing.T) {
	e := newEnv(t)
	if n := e.jwks.Hits(); n != 0 {
		t.Fatalf("hits after NewAuth = %d, want 0 (lazy)", n)
	}
	for i := 0; i < 5; i++ {
		e.requireOK(e.callToken(e.valid()))
	}
	if n := e.jwks.Hits(); n != 1 {
		t.Errorf("hits after 5 requests = %d, want 1 (cached)", n)
	}
	// 時間が経っても、既知の kid では取り直さない。
	e.clock.Advance(time.Hour)
	e.requireOK(e.callToken(e.valid()))
	if n := e.jwks.Hits(); n != 1 {
		t.Errorf("hits = %d, want 1 (known kid never refetches)", n)
	}
}

// 未知の kid → 取り直し → 新しい鍵が現れていたら成功。
// 取り直しの最小間隔の内側では、取り直さずに 401(連打を防ぐ)。
func TestJWKS_UnknownKIDRefetchFindsNewKey(t *testing.T) {
	_, b := testKeys(t)
	e := newEnv(t)
	e.requireOK(e.callToken(e.valid())) // 初回取得(hits=1)
	e.reset()

	// Keycloak 側で鍵が追加された。
	e.jwks.addKey(jwkOf(&b.PublicKey, "sig-2", "sig", "RS256"))
	newKeyToken := func() string { return sign(t, jwt.SigningMethodRS256, b, "sig-2", e.claims()) }

	// 最小間隔の内側(直前の取得から 9.999 秒)。取り直さない。
	e.clock.Advance(10*time.Second - time.Millisecond)
	e.requireUnauthorized(e.callToken(newKeyToken()))
	if n := e.jwks.Hits(); n != 1 {
		t.Fatalf("hits = %d, want 1 (no refetch inside the minimum interval)", n)
	}

	// 間隔が過ぎた(ちょうど 10 秒)。取り直して、新しい鍵で成功。
	e.clock.Advance(time.Millisecond)
	e.requireOK(e.callToken(newKeyToken()))
	if n := e.jwks.Hits(); n != 2 {
		t.Errorf("hits = %d, want 2 (one refetch)", n)
	}
	// 古い鍵も、取り直し後の JWKS に残っているので通る。
	e.requireOK(e.callToken(e.valid()))
}

// 未知の kid が取り直しても現れなければ 401。最小間隔内の連打で取り直しは増えない。
func TestJWKS_UnknownKIDBurstDoesNotRefetch(t *testing.T) {
	_, b := testKeys(t)
	e := newEnv(t)
	ghost := func() string { return sign(t, jwt.SigningMethodRS256, b, "ghost", e.claims()) }

	for i := 0; i < 20; i++ {
		e.requireUnauthorized(e.callToken(ghost()))
	}
	if n := e.jwks.Hits(); n != 1 {
		t.Fatalf("hits after 20 unknown-kid requests = %d, want 1", n)
	}

	// 間隔が過ぎれば、1回だけ取り直す(そしてまた連打では増えない)。
	e.clock.Advance(10 * time.Second)
	for i := 0; i < 20; i++ {
		e.requireUnauthorized(e.callToken(ghost()))
	}
	if n := e.jwks.Hits(); n != 2 {
		t.Errorf("hits = %d, want 2 (one refetch after the interval)", n)
	}
}

// 同時に未知の kid が来ても、取得は1回だけ。
func TestJWKS_ConcurrentColdStartFetchesOnce(t *testing.T) {
	e := newEnv(t)
	token := e.valid()
	var wg sync.WaitGroup
	codes := make([]int, 40)
	for i := range codes {
		wg.Add(1)
		go func() {
			defer wg.Done()
			req := httptest.NewRequest(http.MethodGet, "/", nil)
			req.Header.Set("Authorization", "Bearer "+token)
			rec := httptest.NewRecorder()
			e.handle.ServeHTTP(rec, req)
			codes[i] = rec.Code
		}()
	}
	wg.Wait()
	for i, c := range codes {
		if c != http.StatusNoContent {
			t.Errorf("request %d: status = %d, want 204", i, c)
		}
	}
	if n := e.jwks.Hits(); n != 1 {
		t.Errorf("hits = %d, want 1", n)
	}
}

// 取り直しが成功したら、キャッシュは丸ごと入れ替わる。JWKS から消えた鍵は使えなくなる。
func TestJWKS_RemovedKeyStopsWorkingAfterRefetch(t *testing.T) {
	_, b := testKeys(t)
	e := newEnv(t)
	e.requireOK(e.callToken(e.valid()))
	e.reset()

	e.jwks.setKeys(jwkOf(&b.PublicKey, "sig-2", "sig", "RS256"))
	e.clock.Advance(11 * time.Second)
	e.requireOK(e.callToken(sign(t, jwt.SigningMethodRS256, b, "sig-2", e.claims()))) // 取り直しが起きる
	e.reset()
	e.requireUnauthorized(e.callToken(e.valid())) // sig-1 は消えた
}

// ---- JWKS が取れない → 503 ----

func (e *testEnv) requireUnavailable(rec *httptest.ResponseRecorder) {
	e.t.Helper()
	if rec.Code != http.StatusServiceUnavailable {
		e.t.Fatalf("status = %d, want 503; body=%s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("WWW-Authenticate"); got != "" {
		e.t.Errorf("WWW-Authenticate = %q on 503, want none", got)
	}
	p := decodeProblem(e.t, rec)
	if p.Type != "about:blank" || p.Title != "Service Unavailable" || p.Status != 503 {
		e.t.Errorf("problem = %+v, want about:blank / Service Unavailable / 503", p)
	}
	if e.nextN.Load() != 0 || len(e.users.Subs()) != 0 {
		e.t.Error("a request reached the handler or EnsureUser while verification was impossible")
	}
}

func TestJWKS_ServerDown_503(t *testing.T) {
	e := newEnv(t)
	e.jwks.srv.Close() // 起動時に JWKS が取れない状態(落とさず、使うときに 503)
	e.requireUnavailable(e.callToken(e.valid()))
}

func TestJWKS_BadResponses_503(t *testing.T) {
	cases := map[string]func(s *jwksServer){
		"5xx":         func(s *jwksServer) { s.setStatus(http.StatusInternalServerError) },
		"404":         func(s *jwksServer) { s.setStatus(http.StatusNotFound) },
		"JSON ではない本文": func(s *jwksServer) { s.setBody("<html>Keycloak is starting</html>") },
		"空の本文":        func(s *jwksServer) { s.setBody(" ") },
	}
	for name, breakIt := range cases {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t)
			breakIt(e.jwks)
			e.requireUnavailable(e.callToken(e.valid()))
		})
	}
}

// JWKS が取れたが署名用の鍵が1つも無い場合は、検証不能ではなく「未知の kid」。
func TestJWKS_EmptyKeySet_401(t *testing.T) {
	e := newEnv(t)
	e.jwks.setKeys()
	e.requireUnauthorized(e.callToken(e.valid()))
}

// Keycloak が後から立つ: 最初は 503、間隔内は取り直さず 503、間隔が過ぎて取れたら成功。
func TestJWKS_RecoversAfterKeycloakComesUp(t *testing.T) {
	e := newEnv(t)
	e.jwks.setStatus(http.StatusServiceUnavailable)
	e.requireUnavailable(e.callToken(e.valid()))
	if n := e.jwks.Hits(); n != 1 {
		t.Fatalf("hits = %d, want 1", n)
	}

	e.jwks.setStatus(0) // Keycloak が立った
	for i := 0; i < 5; i++ {
		e.requireUnavailable(e.callToken(e.valid())) // 間隔内: 連打しても取りに行かない
	}
	if n := e.jwks.Hits(); n != 1 {
		t.Fatalf("hits inside the interval = %d, want 1", n)
	}

	e.clock.Advance(10 * time.Second)
	e.requireOK(e.callToken(e.valid()))
	if n := e.jwks.Hits(); n != 2 {
		t.Errorf("hits = %d, want 2", n)
	}
}

// すでにキャッシュにある鍵は、JWKS サーバーが止まっても使える。未知の kid は 503(判断がつかない)。
func TestJWKS_CachedKeySurvivesOutage(t *testing.T) {
	_, b := testKeys(t)
	e := newEnv(t)
	e.requireOK(e.callToken(e.valid()))
	e.reset()

	e.jwks.srv.Close()
	e.clock.Advance(time.Hour)
	e.requireOK(e.callToken(e.valid())) // 既知の kid は取り直さない
	e.reset()

	e.requireUnavailable(e.callToken(sign(t, jwt.SigningMethodRS256, b, "sig-2", e.claims())))
}

// ---- 利用者の用意 ----

func TestAuth_EnsureUserFailure_500(t *testing.T) {
	e := newEnv(t)
	e.users.err = errors.New("db: Error 1045: access denied for user 'slotres'@'10.0.0.5' (password: YES)")
	rec := e.callToken(e.valid())
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
	p := decodeProblem(t, rec)
	if p.Type != "about:blank" || p.Title != "Internal Server Error" || p.Status != 500 {
		t.Errorf("problem = %+v", p)
	}
	if strings.Contains(rec.Body.String(), "Error 1045") || strings.Contains(rec.Body.String(), "password") {
		t.Errorf("500 body leaks the error: %s", rec.Body.String())
	}
	if e.nextN.Load() != 0 {
		t.Error("next handler was called although EnsureUser failed")
	}
	logs := e.logs.String()
	if !strings.Contains(logs, testSub) {
		t.Errorf("log does not carry the sub: %s", logs)
	}
	if strings.Contains(logs, "Error 1045") || strings.Contains(logs, "password") || strings.Contains(logs, "10.0.0.5") {
		t.Errorf("log carries more than the sub (error text leaked): %s", logs)
	}
}

func TestAuth_EnsureUserReceivesSubOnEveryVerifiedRequest(t *testing.T) {
	e := newEnv(t)
	e.requireOK(e.callToken(e.valid(set("sub", "user-a"))))
	e.requireOK(e.callToken(e.valid(set("sub", "user-b"), set("realm_access", map[string]any{"roles": []string{"admin"}}))))
	got := e.users.Subs()
	if len(got) != 2 || got[0] != "user-a" || got[1] != "user-b" {
		t.Errorf("EnsureUser subs = %v, want [user-a user-b]", got)
	}
}

// ---- ログ ----

// 失敗の分類コードと request_id だけを出す。トークン・署名・JWKS の中身は出さない。
func TestAuth_LogsCodesNotSecrets(t *testing.T) {
	a, b := testKeys(t)
	e := newEnv(t)

	expired := e.valid(set("exp", e.clock.Now().Add(-time.Minute).Unix()))
	forged := sign(t, jwt.SigningMethodRS256, b, testKID, e.claims())
	hs := sign(t, jwt.SigningMethodHS256, []byte("secret"), testKID, e.claims())
	e.requireUnauthorized(e.callToken(expired))
	e.requireUnauthorized(e.callToken(forged))
	e.requireUnauthorized(e.callToken(hs))
	e.requireUnauthorized(e.call(""))
	e.jwks.setStatus(http.StatusInternalServerError)
	e.jwks.setKeys()
	e.clock.Advance(time.Hour)
	e.requireUnavailable(e.callToken(sign(t, jwt.SigningMethodRS256, a, "sig-9", e.claims())))

	logs := e.logs.String()
	for _, want := range []string{`"code":"token_expired"`, `"code":"bad_signature"`, `"code":"missing_authorization"`, `"code":"jwks_bad_status"`, `"request_id":`} {
		if !strings.Contains(logs, want) {
			t.Errorf("log lacks %s:\n%s", want, logs)
		}
	}

	secrets := []string{expired, forged, hs, "secret", a.PublicKey.N.String(), base64.RawURLEncoding.EncodeToString(a.PublicKey.N.Bytes())}
	for _, tok := range []string{expired, forged, hs} {
		secrets = append(secrets, strings.Split(tok, ".")...) // ヘッダ・payload・署名の各部分も
	}
	for _, s := range secrets {
		if s != "" && strings.Contains(logs, s) {
			t.Errorf("log contains a secret value (%d chars): %.20s...", len(s), s)
		}
	}
}

// ---- 構成 ----

func TestNewAuth_PanicsOnIncompleteConfig(t *testing.T) {
	base := middleware.AuthConfig{Issuer: testIssuer, JWKSURL: "http://jwks.invalid/certs", Audience: testAudience}
	mustPanic := func(t *testing.T, cfg middleware.AuthConfig, users middleware.UserProvisioner, write middleware.ErrorWriter) {
		t.Helper()
		defer func() {
			if recover() == nil {
				t.Error("NewAuth did not panic")
			}
		}()
		middleware.NewAuth(cfg, users, write, slog.Default())
	}
	cases := map[string]func(c *middleware.AuthConfig){
		"Issuer 空":   func(c *middleware.AuthConfig) { c.Issuer = "" },
		"JWKSURL 空":  func(c *middleware.AuthConfig) { c.JWKSURL = "" },
		"Audience 空": func(c *middleware.AuthConfig) { c.Audience = "" },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			cfg := base
			mutate(&cfg)
			mustPanic(t, cfg, &fakeUsers{}, writeProblem)
		})
	}
	t.Run("UserProvisioner 無し", func(t *testing.T) { mustPanic(t, base, nil, writeProblem) })
	t.Run("ErrorWriter 無し", func(t *testing.T) { mustPanic(t, base, &fakeUsers{}, nil) })
	t.Run("省ける項目(HTTPClient・Now・logger)は省いてよい", func(t *testing.T) {
		if middleware.NewAuth(base, &fakeUsers{}, writeProblem, nil) == nil {
			t.Error("NewAuth returned nil")
		}
	})
}

func TestPrincipalFrom_EmptyContext(t *testing.T) {
	if p, ok := middleware.PrincipalFrom(context.Background()); ok || p != (middleware.Principal{}) {
		t.Errorf("PrincipalFrom(empty) = %+v, %v; want zero, false", p, ok)
	}
}

func TestContextWithPrincipal_RoundTrip(t *testing.T) {
	want := middleware.Principal{Sub: "s", UserID: 7, IsAdmin: true}
	got, ok := middleware.PrincipalFrom(middleware.ContextWithPrincipal(context.Background(), want))
	if !ok || got != want {
		t.Errorf("PrincipalFrom = %+v, %v; want %+v, true", got, ok, want)
	}
}

// 401 の WWW-Authenticate は、ErrorWriter を呼ぶ前に設定されていなければならない(書き始めた後では付けられない)。
func TestAuth_WWWAuthenticateIsSetBeforeErrorWriterRuns(t *testing.T) {
	e := newEnv(t)
	var atWrite string
	var gotStatus int
	e.auth = middleware.NewAuth(middleware.AuthConfig{
		Issuer: testIssuer, JWKSURL: e.jwks.srv.URL, Audience: testAudience,
		HTTPClient: e.jwks.srv.Client(), Now: e.clock.Now,
	}, e.users, func(w http.ResponseWriter, r *http.Request, status int, detail string) {
		atWrite = w.Header().Get("WWW-Authenticate")
		gotStatus = status
		writeProblem(w, r, status, detail)
	}, slog.New(slog.NewJSONHandler(e.logs, nil)))
	e.handle = e.auth.Middleware(http.NotFoundHandler())

	e.requireUnauthorized(e.callToken("garbage"))
	if gotStatus != http.StatusUnauthorized || atWrite != "Bearer" {
		t.Errorf("at ErrorWriter: status=%d WWW-Authenticate=%q; want 401 and Bearer", gotStatus, atWrite)
	}
}

// この層は handler 層を import しない(循環を避ける。家老の訂正)。本体のファイルの import を調べる。
func TestMiddlewarePackageDoesNotImportHandler(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil || len(files) == 0 {
		t.Fatalf("glob: %v, %d files", err, len(files))
	}
	fset := token.NewFileSet()
	for _, f := range files {
		af, err := parser.ParseFile(fset, f, nil, parser.ImportsOnly)
		if err != nil {
			t.Fatalf("parse %s: %v", f, err)
		}
		for _, imp := range af.Imports {
			if strings.HasSuffix(strings.Trim(imp.Path.Value, `"`), "/internal/handler") {
				t.Errorf("%s imports internal/handler", f)
			}
		}
	}
}
