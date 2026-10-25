package middleware

import (
	"context"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"math/big"
	"net/http"
	"sync"
	"time"
)

const (
	// jwksMinRefetchInterval は、JWKS を取りに行く最短の間隔。
	// 未知の kid のトークンを何度も送りつけられても、Keycloak への取得が連打にならないようにする。
	// 代償として、鍵が入れ替わった直後の最大この秒数は、新しい鍵のトークンが 401 になりうる。
	jwksMinRefetchInterval = 10 * time.Second

	// jwksFetchTimeout は、JWKS 1回の取得に待つ上限。
	jwksFetchTimeout = 5 * time.Second

	// maxJWKSBytes は、JWKS の本文として読む上限。
	maxJWKSBytes = 1 << 20

	// minRSAModulusBits は、受け入れる RSA 鍵の最小のビット長。
	minRSAModulusBits = 2048

	// 署名に使う鍵として受ける JWK の条件。use が sig で、alg が RS256 のものだけ。
	// 暗号化用(use=enc・alg=RSA-OAEP など)の鍵では、署名が合っても通さない。
	jwkKeyType  = "RSA"
	jwkUseSig   = "sig"
	jwkAlgRS256 = "RS256"
)

// JWKS を取れなかったときの、失敗の分類コード(ログに出す。応答には出さない)。
const (
	jwksCodeNet  = "jwks_unreachable"
	jwksCodeHTTP = "jwks_bad_status"
	jwksCodeBody = "jwks_bad_body"
)

var (
	// errUnknownKID は、JWKS(取り直した後も)に、署名用の鍵として kid が無いこと。401 にする。
	errUnknownKID = errors.New("unknown kid")
)

// jwksUnavailableError は、JWKS を取れず検証ができないこと。503 にする。
// code は失敗の分類で、取得先の URL や応答の中身は含めない。
type jwksUnavailableError struct{ code string }

func (e *jwksUnavailableError) Error() string { return "jwks unavailable: " + e.code }

// jwkSet と jwk は、JWKS(RFC 7517)のうち、使う項目だけ。
type jwkSet struct {
	Keys []jwk `json:"keys"`
}

type jwk struct {
	Kty string `json:"kty"`
	Kid string `json:"kid"`
	Use string `json:"use"`
	Alg string `json:"alg"`
	N   string `json:"n"`
	E   string `json:"e"`
}

// jwksCache は、JWKS の取得とキャッシュを受け持つ。
//   - 起動時には取りに行かない。最初に必要になったときに取る(Keycloak が後から立ってもよい)。
//   - kid が見つからなければ取り直す。ただし jwksMinRefetchInterval より短い間隔では取り直さない。
//   - 取り直しに成功したら、キャッシュを丸ごと入れ替える(JWKS から消えた鍵は使えなくなる)。
type jwksCache struct {
	url    string
	client *http.Client
	now    func() time.Time

	mu   sync.RWMutex
	keys map[string]*rsa.PublicKey

	// 以下は fetchMu で守る。取得は同時に1つだけ走らせる。
	fetchMu     sync.Mutex
	attempted   bool
	lastAttempt time.Time
	lastErr     *jwksUnavailableError
}

func newJWKSCache(url string, client *http.Client, now func() time.Time) *jwksCache {
	return &jwksCache{url: url, client: client, now: now}
}

// key は、kid の署名用の公開鍵を返す。
// 見つからなければ errUnknownKID、取得できず判断がつかなければ *jwksUnavailableError。
func (c *jwksCache) key(ctx context.Context, kid string) (*rsa.PublicKey, error) {
	if k := c.cached(kid); k != nil {
		return k, nil
	}

	c.fetchMu.Lock()
	defer c.fetchMu.Unlock()

	// 待っている間に、別のリクエストが取り終えているかもしれない。
	if k := c.cached(kid); k != nil {
		return k, nil
	}

	if c.attempted && c.now().Sub(c.lastAttempt) < jwksMinRefetchInterval {
		// 取り直しの間隔内。直前の取得が失敗していたなら検証できない(503)、成功していたなら未知の kid(401)。
		if c.lastErr != nil {
			return nil, c.lastErr
		}
		return nil, errUnknownKID
	}

	c.attempted = true
	c.lastAttempt = c.now()
	keys, err := c.fetch(ctx)
	if err != nil {
		c.lastErr = err
		return nil, err
	}
	c.lastErr = nil

	c.mu.Lock()
	c.keys = keys
	c.mu.Unlock()

	if k := keys[kid]; k != nil {
		return k, nil
	}
	return nil, errUnknownKID
}

func (c *jwksCache) cached(kid string) *rsa.PublicKey {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.keys[kid]
}

// fetch は、JWKS を1回取り、署名用の RSA 鍵だけを kid で引ける形にして返す。
func (c *jwksCache) fetch(ctx context.Context) (map[string]*rsa.PublicKey, *jwksUnavailableError) {
	// 取得はリクエストの取り消しに巻き込まれない。1人が切断しても、他のリクエストが失敗を引き継がないため。
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), jwksFetchTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.url, nil)
	if err != nil {
		return nil, &jwksUnavailableError{code: jwksCodeNet}
	}
	req.Header.Set("Accept", "application/json")

	resp, err := c.client.Do(req)
	if err != nil {
		return nil, &jwksUnavailableError{code: jwksCodeNet}
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, &jwksUnavailableError{code: jwksCodeHTTP}
	}

	var set jwkSet
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxJWKSBytes)).Decode(&set); err != nil {
		return nil, &jwksUnavailableError{code: jwksCodeBody}
	}

	keys := make(map[string]*rsa.PublicKey, len(set.Keys))
	for _, k := range set.Keys {
		if pub := k.signingKey(); pub != nil {
			keys[k.Kid] = pub
		}
	}
	return keys, nil
}

// signingKey は、この JWK が署名検証に使える鍵(RSA・use=sig・alg=RS256・kid あり・2048ビット以上)なら
// 公開鍵を、そうでなければ nil を返す。条件を満たさぬ鍵は、黙って捨てる。
func (k jwk) signingKey() *rsa.PublicKey {
	if k.Kty != jwkKeyType || k.Use != jwkUseSig || k.Alg != jwkAlgRS256 || k.Kid == "" {
		return nil
	}
	nBytes, err := base64.RawURLEncoding.DecodeString(k.N)
	if err != nil {
		return nil
	}
	eBytes, err := base64.RawURLEncoding.DecodeString(k.E)
	if err != nil {
		return nil
	}
	n := new(big.Int).SetBytes(nBytes)
	e := new(big.Int).SetBytes(eBytes)
	if n.BitLen() < minRSAModulusBits || !e.IsInt64() || e.Int64() < 3 || e.Int64() > 1<<31-1 {
		return nil
	}
	return &rsa.PublicKey{N: n, E: int(e.Int64())}
}
