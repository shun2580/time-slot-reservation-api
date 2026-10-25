package handler

import (
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
)

// route は、登録された1本のルート。
type route struct {
	pattern   string // Go 1.22 形式("GET /healthz")
	handler   http.Handler
	protected bool
}

// Router は、全ルートを束ねる。認証なしの Public と、認証ミドルウェアを通す Protected を登録し、
// Handler() で http.Handler に組み上げる。登録の検査(重複・不正な pattern・認証の未設定)は Handler() でまとめて行う。
type Router struct {
	routes []route
	auth   func(http.Handler) http.Handler
}

// NewRouter は、空の Router を返す。
func NewRouter() *Router {
	return &Router{}
}

// Public は、認証なしのルートを登録する(healthz など)。pattern は "GET /healthz" の形。
func (r *Router) Public(pattern string, h http.Handler) {
	r.routes = append(r.routes, route{pattern: pattern, handler: h})
}

// Protected は、SetAuth のミドルウェアを通すルートを登録する。
func (r *Router) Protected(pattern string, h http.Handler) {
	r.routes = append(r.routes, route{pattern: pattern, handler: h, protected: true})
}

// SetAuth は、Protected のルートに掛けるミドルウェアを設定する。Public には掛からない。
func (r *Router) SetAuth(mw func(http.Handler) http.Handler) {
	r.auth = mw
}

// Handler は、登録されたルートを http.Handler に組み上げる。
//
// 認証は、登録済みの Protected ルートに一致した要求にだけ掛かる。
//   - 未定義のパス: 認証より前に 404 の Problem Details を返す。
//   - 定義済みのパスでメソッドが違う: 認証より前に 405 + Allow(許可メソッドをソート済み)の Problem Details を返す。
//
// Protected があるのに SetAuth が無い場合、pattern が不正・重複している場合は、panic せずエラーを返す。
func (r *Router) Handler() (h http.Handler, err error) {
	hasProtected := false
	for _, rt := range r.routes {
		if rt.protected {
			hasProtected = true
			break
		}
	}
	if hasProtected && r.auth == nil {
		return nil, errors.New("Protected のルートがあるが SetAuth が呼ばれていない")
	}

	// ServeMux.Handle は、不正な pattern・重複・nil のハンドラで panic する。エラーに変えて返す。
	defer func() {
		if rec := recover(); rec != nil {
			h, err = nil, fmt.Errorf("ルートの登録に失敗した: %v", rec)
		}
	}()

	mux := http.NewServeMux()
	for _, rt := range r.routes {
		if rt.handler == nil {
			return nil, fmt.Errorf("ハンドラが nil: %q", rt.pattern)
		}
		hh := rt.handler
		if rt.protected {
			hh = r.auth(hh)
		}
		mux.Handle(rt.pattern, hh)
	}
	return &routerHandler{mux: mux}, nil
}

// routerHandler は、ServeMux の前に立ち、404 と 405 を Problem Details にする。
//
// "/" の catch-all を ServeMux に登録すると、パスが一致してメソッドだけが違う要求も "/" に当たり、
// 405 にならず 404 になってしまう。そこで catch-all は登録せず、一致しなかった要求を自分で 404 と 405 に分ける。
type routerHandler struct {
	mux *http.ServeMux
}

// probeMethods は、許可メソッドを調べるときに試すメソッド。
var probeMethods = []string{
	http.MethodGet, http.MethodHead, http.MethodPost, http.MethodPut, http.MethodPatch,
	http.MethodDelete, http.MethodOptions, http.MethodConnect, http.MethodTrace,
}

func (rh *routerHandler) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	if _, pattern := rh.mux.Handler(req); pattern != "" {
		// PathValue を設定させるため、取り出したハンドラではなく mux 経由で呼ぶ。
		rh.mux.ServeHTTP(w, req)
		return
	}

	// 一致なし。メソッドを変えれば一致するなら 405、どれでも一致しなければ 404。
	// 照合は ServeMux 自身に任せる(GET の登録は HEAD にも一致する、といった規則を写さない)。
	var allowed []string
	for _, m := range probeMethods {
		probe := req.Clone(req.Context())
		probe.Method = m
		if _, p := rh.mux.Handler(probe); p != "" {
			allowed = append(allowed, m)
		}
	}
	if len(allowed) == 0 {
		NotFound(w, req)
		return
	}
	sort.Strings(allowed)
	w.Header().Set("Allow", strings.Join(allowed, ", "))
	WriteStatusProblem(w, req, http.StatusMethodNotAllowed, "")
}
