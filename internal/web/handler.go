// Package web provides the HTTP surface for the application UI.
package web

import (
	"context"
	"crypto/subtle"
	"errors"
	"html/template"
	"io"
	"io/fs"
	"mime"
	"net/http"
	"net/mail"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	ui "namo/web"
)

type pageData struct {
	Title          string
	Active         string
	State          string
	Saved          bool
	Deleted        bool
	UserName       string
	TenantName     string
	Notices        []noticeView
	Notice         noticeView
	Filters        []filterView
	Recipients     []recipientView
	Reports        []ReportView
	Members        []memberView
	Tenants        []tenantView
	CurrentDate    string
	SearchQuery    string
	SelectedFilter string
	Category       string
	Region         string
	Pagination     paginationView
	Role           string
	CSRFToken      string
	Writable       bool
	Dashboard      DashboardView
	Demo           bool
	LoginEnabled   bool
	DeliveryTime   string
	Timezone       string
	ContactEmail   string
	Admin          AdminView
	DeliveryDays   []int
	AdminWritable  bool
	AdminResult    string
	ReportEmpty    bool
	InviteResult   string
	Invitation     InvitationView
	InviteExpires  string
	InviteToken    string
	SignupEnabled  bool
	Accounts       []AccountView
	TenantOptions  []TenantOption
}

// RequestContext is the authenticated identity supplied by the outer app.
type RequestContext struct {
	UserID     string
	UserName   string
	Email      string
	TenantName string
	TenantID   string
	Role       string
	CSRFToken  string
}

// RequestContextMapper maps auth middleware state into the web view context.
type RequestContextMapper func(*http.Request) (RequestContext, error)

// DashboardView contains the observable collection summary.
type DashboardView struct {
	LastCollected string
	Collected     int
	NewNotices    int
	Matches       int
	ActiveFilters int
	RunTime       string
	NextDelivery  string
	Healthy       bool
}

// AdminView contains platform collection health.
type AdminView struct {
	Healthy        bool
	LastCollected  string
	CollectedCount int
	FailedJobs     int
	ReportDir      string
}

// ReportView is one tenant-visible report artifact and its generation state.
type ReportView struct {
	ID, FileName, Trigger, Status, DueAt, GeneratedAt string
	NoticeCount                                       int
	Downloadable                                      bool
}

// ReportDownload is an opened generated report ready for HTTP delivery.
type ReportDownload struct {
	Name     string
	Modified time.Time
	Body     io.ReadSeekCloser
}

// NoticeView is a notice row/detail prepared by integration.
type NoticeView struct {
	SourceKind     string
	Keyword        string
	Trade          string
	CollectedDate  string
	CollectedClock string
	PostedAt       string
	FilterKeywords map[string]string
	ID             string
	Title          string
	Category       string
	Agency         string
	Region         string
	Amount         string
	Deadline       string
	SourceURL      string
	Reasons        []string
	FilterReasons  map[string][]string
}

type noticeView = NoticeView

type paginationView struct {
	Page, Pages, PageSize, Total int
	PreviousURL, NextURL         string
}

// FilterView is a tenant filter prepared by integration.
type FilterView struct {
	ID      string
	Name    string
	Summary string
	Matches int
	Enabled bool
}

type filterView = FilterView

// RecipientView is a digest recipient prepared by integration.
type RecipientView struct {
	Name  string
	Email string
	State string
}

type recipientView = RecipientView

// MemberView is a tenant member prepared by integration.
type MemberView struct {
	UserID    string
	Name      string
	Email     string
	Role      string
	Removable bool
}

type memberView = MemberView

// TenantView is a platform tenant prepared by integration.
type TenantView struct {
	Name        string
	Members     int
	LastDigest  string
	State       string
	AdminName   string
	AdminEmail  string
	ContactMail string
}

type tenantView = TenantView

// AccountView is one member account and its tenant assignment as shown on the
// platform administrator screen.
type AccountView struct {
	UserID      string
	Email       string
	DisplayName string
	TenantID    string
	TenantName  string
	Created     string
	Role        string
	RoleLabel   string
	Assigned    bool
}

// TenantOption is one assignable tenant.
type TenantOption struct {
	ID   string
	Name string
}

// AppData is the read model loaded for server-rendered pages.
type AppData struct {
	Dashboard     DashboardView
	Notices       []NoticeView
	Filters       []FilterView
	Recipients    []RecipientView
	Reports       []ReportView
	Members       []MemberView
	Tenants       []TenantView
	Accounts      []AccountView
	TenantOptions []TenantOption
	DeliveryTime  string
	DeliveryDays  []int
	Timezone      string
	ContactEmail  string
	Admin         AdminView
	Demo          bool
}

// PageRequest identifies the read surface for selective backend loading.
type PageRequest struct {
	Path string
}

// Backend loads tenant-aware read models.
type Backend interface {
	Load(context.Context, RequestContext, PageRequest) (AppData, error)
}

// FilterCommand is a validated filter-save request.
type FilterCommand struct {
	Name            string
	IncludeKeywords string
	IncludeMode     string
	ExcludeKeywords string
	Category        string
	Region          string
	MinimumAmount   *int64
	DeadlineDays    *int
	Agency          string
}

// ToggleFilterCommand is a validated filter-state request.
type ToggleFilterCommand struct {
	FilterID string
	Enabled  bool
}

// DeleteFilterCommand identifies one tenant-owned filter to remove.
type DeleteFilterCommand struct {
	FilterID string
}

// NotificationCommand is a validated digest schedule request.
type NotificationCommand struct {
	DeliveryTime string
	DeliveryDays []int
	Timezone     string
}

// RecipientCommand is a validated digest-recipient request.
type RecipientCommand struct {
	Name  string
	Email string
}

// AssignAccountCommand grants company access with a role, or revokes access
// when TenantID is empty.
type AssignAccountCommand struct {
	UserID   string
	TenantID string
	Role     string
}

// AccountCommand names one target account.
type AccountCommand struct {
	UserID string
}

// SettingsCommand is a validated tenant settings request.
type SettingsCommand struct {
	TenantName   string
	ContactEmail string
}

var ErrTenantExists = errors.New("tenant is already registered")
var ErrAccountRole = errors.New("account role requires a company")
var ErrInvitationUnavailable = errors.New("invitation is unavailable")
var ErrInvitationMailDelivery = errors.New("invitation was saved but mail delivery failed")
var ErrInvitationPending = errors.New("invitation is already pending")
var ErrReportNotFound = errors.New("report is unavailable")
var ErrNoReportMatches = errors.New("no eligible report matches")

// TenantCommand registers one company and its administrator contact.
type TenantCommand struct {
	Name, ContactEmail, AdminName, AdminEmail string
}

// MemberInviteCommand creates or replaces an invitation inside one tenant.
type MemberInviteCommand struct {
	Name, Email, Role string
}

type AcceptInviteCommand struct {
	Token, DisplayName, Password string
}

type InvitationView struct {
	TenantName, Email, DisplayName, Role string
	ExpiresAt                            time.Time
}

type InvitationResult struct {
	URL       string
	ExpiresAt time.Time
}

// Onboarding is the public and administrator invitation boundary.
type Onboarding interface {
	Invitation(context.Context, string) (InvitationView, error)
	AcceptInvitation(context.Context, AcceptInviteCommand) error
}

// Actions applies validated tenant-aware mutation commands.
type Actions interface {
	SaveFilter(context.Context, RequestContext, FilterCommand) error
	ToggleFilter(context.Context, RequestContext, ToggleFilterCommand) error
	DeleteFilter(context.Context, RequestContext, DeleteFilterCommand) error
	SaveNotification(context.Context, RequestContext, NotificationCommand) error
	SaveSettings(context.Context, RequestContext, SettingsCommand) error
	AssignAccountTenant(context.Context, RequestContext, AssignAccountCommand) error
	CreateTenant(context.Context, RequestContext, TenantCommand) error
	RemoveMember(context.Context, RequestContext, AccountCommand) error
	DeleteAccount(context.Context, RequestContext, AccountCommand) error
	AddRecipient(context.Context, RequestContext, RecipientCommand) error
	RunCollection(context.Context, RequestContext) error
	SendTestMail(context.Context, RequestContext) error
	SaveReportSchedule(context.Context, RequestContext, NotificationCommand) error
	GenerateReport(context.Context, RequestContext) error
	RetryReport(context.Context, RequestContext, string) error
	OpenReport(context.Context, RequestContext, string) (ReportDownload, error)
}

// Options configures the production handler behind authentication middleware.
type Options struct {
	Backend    Backend
	Actions    Actions
	Onboarding Onboarding
	MapContext RequestContextMapper
}

// Handler serves the embeddable web interface. Domain integration can replace
// the local page models without changing template routes.
type Handler struct {
	templates  *template.Template
	assets     http.Handler
	backend    Backend
	actions    Actions
	onboarding Onboarding
	mapContext RequestContextMapper
	demo       bool
}

// NewHandler returns a read-only sample-data handler for browser QA.
func NewHandler() http.Handler {
	handler, err := newHandler(Options{
		Backend: sampleBackend{},
		MapContext: func(*http.Request) (RequestContext, error) {
			return RequestContext{UserName: "김담당", TenantName: "샘플 주식회사", Role: "platform_admin"}, nil
		},
	}, true)
	if err != nil {
		panic(err)
	}
	return handler
}

// NewHandlerWithOptions returns a production handler. Authentication remains
// an outer middleware responsibility; this handler never verifies credentials.
func NewHandlerWithOptions(options Options) (http.Handler, error) {
	if options.Backend == nil || options.Actions == nil || options.MapContext == nil {
		return nil, errors.New("web: Backend, Actions, and MapContext are required")
	}
	return newHandler(options, false)
}

func newHandler(options Options, demo bool) (http.Handler, error) {
	templates := template.Must(template.New("").Funcs(template.FuncMap{
		"firstReason": firstReason,
		"initial":     initial,
		"safeURL":     safeURL,
		"number":      formatNumber,
		"hasDay":      hasDay,
		"weekdays":    weekdays,
	}).ParseFS(ui.Files, "templates/*.html"))
	assetsFS, err := fs.Sub(ui.Files, "static")
	if err != nil {
		return nil, err
	}
	return &Handler{
		templates:  templates,
		assets:     http.FileServer(http.FS(assetsFS)),
		backend:    options.Backend,
		actions:    options.Actions,
		onboarding: options.Onboarding,
		mapContext: options.MapContext,
		demo:       demo,
	}, nil
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if strings.HasPrefix(r.URL.Path, "/assets/") {
		if !allows(r.Method, http.MethodGet, http.MethodHead) {
			methodNotAllowed(w, http.MethodGet, http.MethodHead)
			return
		}
		h.serveAsset(w, r)
		return
	}
	requestContext, err := h.mapContext(r)
	if err != nil {
		http.Error(w, "사용자 정보를 확인하지 못했습니다.", http.StatusInternalServerError)
		return
	}
	if strings.HasPrefix(r.URL.Path, "/admin") && !canViewAdmin(requestContext) {
		http.Error(w, "플랫폼 관리자 권한이 필요합니다.", http.StatusForbidden)
		return
	}
	// A self-service account carries no tenant until a platform administrator
	// assigns one, so it must not reach tenant screens or commands.
	if awaitingTenant(requestContext) {
		h.renderPending(w, r, requestContext)
		return
	}
	if r.URL.Path == "/notifications" {
		if !allows(r.Method, http.MethodGet, http.MethodHead) {
			methodNotAllowed(w, http.MethodGet, http.MethodHead)
			return
		}
		http.Redirect(w, r, "/reports", http.StatusSeeOther)
		return
	}
	downloadReportID, downloadRoute := reportRouteID(r.URL.Path, "download")
	retryReportID, retryRoute := reportRouteID(r.URL.Path, "retry")
	var appData AppData
	if allows(r.Method, http.MethodGet, http.MethodHead) && !downloadRoute && r.URL.Path != "/login" && r.URL.Path != "/signup" && r.URL.Path != "/accept-invite" && r.URL.Path != "/" {
		appData, err = h.backend.Load(r.Context(), requestContext, PageRequest{Path: r.URL.Path})
		if err != nil {
			http.Error(w, "화면 데이터를 불러오지 못했습니다.", http.StatusInternalServerError)
			return
		}
	}

	switch {
	case r.URL.Path == "/":
		if !allows(r.Method, http.MethodGet, http.MethodHead) {
			methodNotAllowed(w, http.MethodGet, http.MethodHead)
			return
		}
		http.Redirect(w, r, "/dashboard", http.StatusSeeOther)
	case r.URL.Path == "/login":
		if r.Method == http.MethodPost {
			if h.demo {
				demoNotImplemented(w)
				return
			}
			methodNotAllowed(w, http.MethodGet, http.MethodHead)
			return
		}
		if !allows(r.Method, http.MethodGet, http.MethodHead) {
			methodNotAllowed(w, http.MethodGet, http.MethodHead)
			return
		}
		data := page("로그인", "", requestContext, canMutateTenant(requestContext), appData.Demo)
		data.LoginEnabled = !h.demo
		data.SignupEnabled = !h.demo
		if !h.demo && r.URL.Query().Get("accepted") == "1" {
			data.InviteResult = "계정을 만들었습니다. 새 비밀번호로 로그인하세요."
		}
		h.render(w, "login", data)
	case r.URL.Path == "/signup":
		if r.Method == http.MethodPost {
			// The authentication middleware owns account creation. A POST only
			// arrives here when that middleware is absent, as in the demo.
			if h.demo {
				demoNotImplemented(w)
				return
			}
			methodNotAllowed(w, http.MethodGet, http.MethodHead)
			return
		}
		if !allows(r.Method, http.MethodGet, http.MethodHead) {
			methodNotAllowed(w, http.MethodGet, http.MethodHead)
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		data := page("회원가입", "", requestContext, false, appData.Demo)
		data.SignupEnabled = !h.demo
		h.render(w, "signup", data)
	case r.URL.Path == "/accept-invite":
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Cache-Control", "no-store")
		if h.demo || h.onboarding == nil {
			h.renderStatus(w, http.StatusServiceUnavailable, "초대 기능을 사용할 수 없습니다", "관리자에게 문의해 주세요.")
			return
		}
		if r.Method == http.MethodPost {
			if r.FormValue("mode") == "inspect" {
				h.handleInspectInvitation(w, r, requestContext)
			} else {
				h.handleAcceptInvitation(w, r, requestContext)
			}
			return
		}
		if !allows(r.Method, http.MethodGet, http.MethodHead) {
			methodNotAllowed(w, http.MethodGet, http.MethodHead, http.MethodPost)
			return
		}
		if r.URL.RawQuery != "" {
			h.renderStatus(w, http.StatusBadRequest, "초대 링크를 확인해 주세요", "메일에 있는 최신 초대 링크를 다시 열어 주세요.")
			return
		}
		data := page("초대 수락", "", requestContext, false, false)
		h.render(w, "accept-invite-bootstrap", data)
	case r.URL.Path == "/dashboard":
		if !allows(r.Method, http.MethodGet, http.MethodHead) {
			methodNotAllowed(w, http.MethodGet, http.MethodHead)
			return
		}
		data := page("입찰공고 모니터링", "dashboard", requestContext, canMutateTenant(requestContext), appData.Demo)
		data.Dashboard = appData.Dashboard
		h.render(w, "dashboard", data)
	case r.URL.Path == "/notices":
		if !allows(r.Method, http.MethodGet, http.MethodHead) {
			methodNotAllowed(w, http.MethodGet, http.MethodHead)
			return
		}
		data := page("공고 목록", "notices", requestContext, canMutateTenant(requestContext), appData.Demo)
		data.State = state(r)
		data.SearchQuery = strings.TrimSpace(r.URL.Query().Get("q"))
		data.SelectedFilter = strings.TrimSpace(r.URL.Query().Get("filter"))
		data.Category = r.URL.Query().Get("category")
		data.Region = r.URL.Query().Get("region")
		data.Filters = appData.Filters
		filtered := filterNotices(appData.Notices, data.SearchQuery, data.SelectedFilter, data.Category, data.Region)
		data.Notices, data.Pagination = paginateNotices(filtered, r.URL.Query())
		if data.State == "empty" {
			data.Notices = nil
		}
		h.render(w, "notices", data)
	case strings.HasPrefix(r.URL.Path, "/notices/"):
		if !allows(r.Method, http.MethodGet, http.MethodHead) {
			methodNotAllowed(w, http.MethodGet, http.MethodHead)
			return
		}
		id := strings.TrimPrefix(r.URL.Path, "/notices/")
		notice, ok := findNotice(appData.Notices, id)
		if !ok {
			h.renderStatus(w, http.StatusNotFound, "찾을 수 없는 공고", "공고 번호를 확인하거나 목록으로 돌아가 주세요.")
			return
		}
		data := page("공고 상세", "notices", requestContext, canMutateTenant(requestContext), appData.Demo)
		data.SelectedFilter = strings.TrimSpace(r.URL.Query().Get("filter"))
		if data.SelectedFilter != "" {
			filtered := filterNotices([]noticeView{notice}, "", data.SelectedFilter, "", "")
			if len(filtered) == 0 {
				h.renderStatus(w, http.StatusNotFound, "필터에 없는 공고", "선택한 필터의 공고 목록으로 돌아가 주세요.")
				return
			}
			notice = filtered[0]
		}
		data.Notice = notice
		h.render(w, "notice-detail", data)
	case r.URL.Path == "/filters":
		if r.Method == http.MethodPost {
			if h.demo {
				demoNotImplemented(w)
				return
			}
			if !canMutateTenant(requestContext) {
				http.Error(w, "테넌트 관리자 권한이 필요합니다.", http.StatusForbidden)
				return
			}
			h.handleSaveFilter(w, r, requestContext)
			return
		}
		if !allows(r.Method, http.MethodGet, http.MethodHead) {
			methodNotAllowed(w, http.MethodGet, http.MethodHead, http.MethodPost)
			return
		}
		data := page("필터 관리", "filters", requestContext, canMutateTenant(requestContext), appData.Demo)
		data.Saved = !h.demo && r.URL.Query().Get("saved") == "1"
		data.Deleted = !h.demo && r.URL.Query().Get("deleted") == "1"
		data.Filters = appData.Filters
		h.render(w, "filters", data)
	case r.URL.Path == "/filters/toggle":
		if r.Method != http.MethodPost {
			methodNotAllowed(w, http.MethodPost)
			return
		}
		if h.demo {
			demoNotImplemented(w)
			return
		}
		if !canMutateTenant(requestContext) {
			http.Error(w, "테넌트 관리자 권한이 필요합니다.", http.StatusForbidden)
			return
		}
		h.handleFilterToggle(w, r, requestContext)
	case r.URL.Path == "/filters/delete":
		if r.Method != http.MethodPost {
			methodNotAllowed(w, http.MethodPost)
			return
		}
		if h.demo {
			demoNotImplemented(w)
			return
		}
		if requestContext.TenantID == "" || requestContext.Role != "tenant_admin" {
			http.Error(w, "테넌트 관리자 권한이 필요합니다.", http.StatusForbidden)
			return
		}
		h.handleFilterDelete(w, r, requestContext)
	case r.URL.Path == "/reports":
		if r.Method == http.MethodPost {
			if h.demo {
				demoNotImplemented(w)
				return
			}
			if !canMutateReport(requestContext) {
				http.Error(w, "테넌트 관리자 권한이 필요합니다.", http.StatusForbidden)
				return
			}
			h.handleSaveReportSchedule(w, r, requestContext)
			return
		}
		if !allows(r.Method, http.MethodGet, http.MethodHead) {
			methodNotAllowed(w, http.MethodGet, http.MethodHead, http.MethodPost)
			return
		}
		data := page("리포트", "reports", requestContext, canMutateReport(requestContext), appData.Demo)
		data.State = state(r)
		data.Saved = !h.demo && r.URL.Query().Get("saved") == "1"
		data.Reports = appData.Reports
		if data.State == "empty" {
			data.Reports = nil
		}
		data.DeliveryTime = appData.DeliveryTime
		data.DeliveryDays = appData.DeliveryDays
		data.Timezone = appData.Timezone
		data.Dashboard = appData.Dashboard
		switch r.URL.Query().Get("result") {
		case "generated":
			data.AdminResult = "리포트 생성을 요청했습니다."
		case "empty":
			data.ReportEmpty = true
		case "retried":
			data.AdminResult = "리포트 재시도를 요청했습니다."
		}
		h.render(w, "reports", data)
	case r.URL.Path == "/reports/generate":
		if r.Method != http.MethodPost {
			methodNotAllowed(w, http.MethodPost)
			return
		}
		if h.demo {
			demoNotImplemented(w)
			return
		}
		if !canMutateReport(requestContext) {
			http.Error(w, "테넌트 관리자 권한이 필요합니다.", http.StatusForbidden)
			return
		}
		h.handleGenerateReport(w, r, requestContext)
	case retryRoute:
		if r.Method != http.MethodPost {
			methodNotAllowed(w, http.MethodPost)
			return
		}
		if h.demo {
			demoNotImplemented(w)
			return
		}
		if !canMutateReport(requestContext) {
			http.Error(w, "테넌트 관리자 권한이 필요합니다.", http.StatusForbidden)
			return
		}
		h.handleRetryReport(w, r, requestContext, retryReportID)
	case downloadRoute:
		if !allows(r.Method, http.MethodGet, http.MethodHead) {
			methodNotAllowed(w, http.MethodGet, http.MethodHead)
			return
		}
		if h.demo {
			demoNotImplemented(w)
			return
		}
		h.handleReportDownload(w, r, requestContext, downloadReportID)
	case r.URL.Path == "/settings":
		if r.Method == http.MethodPost {
			if h.demo {
				demoNotImplemented(w)
				return
			}
			if !canMutateTenant(requestContext) {
				http.Error(w, "테넌트 관리자 권한이 필요합니다.", http.StatusForbidden)
				return
			}
			h.handleSaveSettings(w, r, requestContext)
			return
		}
		if !allows(r.Method, http.MethodGet, http.MethodHead) {
			methodNotAllowed(w, http.MethodGet, http.MethodHead, http.MethodPost)
			return
		}
		data := page("환경 설정", "settings", requestContext, canMutateTenant(requestContext), appData.Demo)
		data.Saved = !h.demo && r.URL.Query().Get("saved") == "1"
		data.Members = removableMembers(appData.Members, requestContext)
		data.ContactEmail = appData.ContactEmail
		if r.URL.Query().Get("result") == "member-removed" {
			data.InviteResult = "구성원을 회사에서 제외했습니다."
		}
		h.render(w, "settings", data)
	case r.URL.Path == "/settings/members":
		if r.Method != http.MethodPost {
			methodNotAllowed(w, http.MethodPost)
			return
		}
		if h.demo {
			demoNotImplemented(w)
			return
		}
		if requestContext.Role != "tenant_admin" || requestContext.TenantID == "" {
			http.Error(w, "회사 관리자 권한이 필요합니다.", http.StatusForbidden)
			return
		}
		h.handleRemoveMember(w, r, requestContext)
	case r.URL.Path == "/admin/tenants":
		if r.Method != http.MethodPost {
			methodNotAllowed(w, http.MethodPost)
			return
		}
		if h.demo {
			demoNotImplemented(w)
			return
		}
		h.handleCreateTenant(w, r, requestContext)
	case r.URL.Path == "/admin/accounts/delete":
		if r.Method != http.MethodPost {
			methodNotAllowed(w, http.MethodPost)
			return
		}
		if h.demo {
			demoNotImplemented(w)
			return
		}
		h.handleDeleteAccount(w, r, requestContext)
	case r.URL.Path == "/admin/accounts":
		if r.Method != http.MethodPost {
			methodNotAllowed(w, http.MethodPost)
			return
		}
		if h.demo {
			demoNotImplemented(w)
			return
		}
		h.handleAssignAccount(w, r, requestContext)
	case r.URL.Path == "/admin/collect":
		if r.Method != http.MethodPost {
			methodNotAllowed(w, http.MethodPost)
			return
		}
		if h.demo {
			demoNotImplemented(w)
			return
		}
		h.handlePlatformAction(w, r, requestContext, "collection")
	case r.URL.Path == "/admin":
		if !allows(r.Method, http.MethodGet, http.MethodHead) {
			methodNotAllowed(w, http.MethodGet, http.MethodHead)
			return
		}
		data := page("플랫폼 관리", "admin", requestContext, canMutateTenant(requestContext), appData.Demo)
		data.Tenants = appData.Tenants
		data.Accounts = appData.Accounts
		data.TenantOptions = appData.TenantOptions
		data.Admin = appData.Admin
		data.AdminWritable = !h.demo
		switch r.URL.Query().Get("result") {
		case "collection":
			data.AdminResult = "수집 작업을 시작했습니다."
		case "tenant-created":
			data.AdminResult = "회사를 등록했습니다. 회원 계정 배정에서 선택할 수 있습니다."
		case "account-assigned":
			data.AdminResult = "계정의 회사와 권한을 반영했습니다."
		case "account-revoked":
			data.AdminResult = "계정의 회사 배정을 해제했습니다."
		case "account-deleted":
			data.AdminResult = "계정을 삭제했습니다. 해당 계정의 로그인 세션도 함께 종료되었습니다."
		}
		h.render(w, "admin", data)
	default:
		h.renderStatus(w, http.StatusNotFound, "찾을 수 없는 페이지", "주소를 확인하거나 대시보드로 이동해 주세요.")
	}
}

func (h *Handler) handleSaveFilter(w http.ResponseWriter, r *http.Request, requestContext RequestContext) {
	if !validCSRF(r, requestContext) {
		http.Error(w, "요청을 확인할 수 없습니다.", http.StatusForbidden)
		return
	}
	var deadlineDays *int
	if raw := strings.TrimSpace(r.FormValue("deadline_days")); raw != "" {
		days, err := strconv.Atoi(raw)
		if err != nil || days < 1 || days > 365 {
			http.Error(w, "마감 여유일은 1~365 사이로 입력하거나 비워 주세요.", http.StatusBadRequest)
			return
		}
		deadlineDays = &days
	}
	var minimumAmount *int64
	if raw := strings.TrimSpace(r.FormValue("min_amount")); raw != "" {
		amount, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || amount < 0 {
			http.Error(w, "최소 금액을 확인해 주세요.", http.StatusBadRequest)
			return
		}
		minimumAmount = &amount
	}
	command := FilterCommand{
		Name:            strings.TrimSpace(r.FormValue("name")),
		IncludeKeywords: strings.TrimSpace(r.FormValue("include_keywords")),
		IncludeMode:     strings.TrimSpace(r.FormValue("include_mode")),
		ExcludeKeywords: strings.TrimSpace(r.FormValue("exclude_keywords")),
		Category:        r.FormValue("category"),
		Region:          strings.TrimSpace(r.FormValue("region")),
		MinimumAmount:   minimumAmount,
		DeadlineDays:    deadlineDays,
		Agency:          strings.TrimSpace(r.FormValue("agency")),
	}
	if command.IncludeMode == "" {
		command.IncludeMode = "any"
	}
	if command.Name == "" ||
		!allowedValue(command.IncludeMode, "any", "all") ||
		!allowedValue(command.Category, "공사", "용역", "물품", "외자") ||
		utf8.RuneCountInString(command.Region) > 128 {
		http.Error(w, "필터 이름과 마감 여유일을 확인해 주세요.", http.StatusBadRequest)
		return
	}
	if err := h.actions.SaveFilter(r.Context(), requestContext, command); err != nil {
		http.Error(w, "필터를 저장하지 못했습니다.", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/filters?saved=1", http.StatusSeeOther)
}

func allowedValue(value string, allowed ...string) bool {
	if value == "" {
		return true
	}
	for _, candidate := range allowed {
		if value == candidate {
			return true
		}
	}
	return false
}

func (h *Handler) handleFilterToggle(w http.ResponseWriter, r *http.Request, requestContext RequestContext) {
	if !validCSRF(r, requestContext) {
		http.Error(w, "요청을 확인할 수 없습니다.", http.StatusForbidden)
		return
	}
	command := ToggleFilterCommand{
		FilterID: strings.TrimSpace(r.FormValue("filter")),
		Enabled:  r.FormValue("enabled") == "1",
	}
	if command.FilterID == "" {
		http.Error(w, "필터 번호가 올바르지 않습니다.", http.StatusBadRequest)
		return
	}
	if err := h.actions.ToggleFilter(r.Context(), requestContext, command); err != nil {
		http.Error(w, "필터 상태를 저장하지 못했습니다.", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/filters?saved=1", http.StatusSeeOther)
}

func (h *Handler) handleFilterDelete(w http.ResponseWriter, r *http.Request, requestContext RequestContext) {
	if !validCSRF(r, requestContext) {
		http.Error(w, "요청을 확인할 수 없습니다.", http.StatusForbidden)
		return
	}
	command := DeleteFilterCommand{FilterID: strings.TrimSpace(r.FormValue("filter"))}
	if command.FilterID == "" {
		http.Error(w, "필터 번호가 올바르지 않습니다.", http.StatusBadRequest)
		return
	}
	if err := h.actions.DeleteFilter(r.Context(), requestContext, command); err != nil {
		http.Error(w, "필터를 삭제하지 못했습니다.", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/filters?deleted=1", http.StatusSeeOther)
}

func (h *Handler) handleSaveReportSchedule(w http.ResponseWriter, r *http.Request, requestContext RequestContext) {
	if !validCSRF(r, requestContext) {
		http.Error(w, "요청을 확인할 수 없습니다.", http.StatusForbidden)
		return
	}
	deliveryDays, err := parseDeliveryDays(r)
	if err != nil {
		http.Error(w, "생성 요일을 하나 이상 선택해 주세요.", http.StatusBadRequest)
		return
	}
	command := NotificationCommand{DeliveryTime: r.FormValue("delivery_time"), DeliveryDays: deliveryDays, Timezone: r.FormValue("timezone")}
	if _, err := time.Parse("15:04", command.DeliveryTime); err != nil {
		http.Error(w, "생성 시각을 확인해 주세요.", http.StatusBadRequest)
		return
	}
	if _, err := time.LoadLocation(command.Timezone); err != nil {
		http.Error(w, "시간대를 확인해 주세요.", http.StatusBadRequest)
		return
	}
	if err := h.actions.SaveReportSchedule(r.Context(), requestContext, command); err != nil {
		http.Error(w, "리포트 일정을 저장하지 못했습니다.", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/reports?saved=1", http.StatusSeeOther)
}

func (h *Handler) handleGenerateReport(w http.ResponseWriter, r *http.Request, requestContext RequestContext) {
	if !validCSRF(r, requestContext) {
		http.Error(w, "요청을 확인할 수 없습니다.", http.StatusForbidden)
		return
	}
	if err := h.actions.GenerateReport(r.Context(), requestContext); errors.Is(err, ErrNoReportMatches) {
		http.Redirect(w, r, "/reports?result=empty", http.StatusSeeOther)
		return
	} else if err != nil {
		http.Error(w, "리포트를 생성하지 못했습니다.", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/reports?result=generated", http.StatusSeeOther)
}

func (h *Handler) handleRetryReport(w http.ResponseWriter, r *http.Request, requestContext RequestContext, reportID string) {
	if !validCSRF(r, requestContext) {
		http.Error(w, "요청을 확인할 수 없습니다.", http.StatusForbidden)
		return
	}
	if err := h.actions.RetryReport(r.Context(), requestContext, reportID); err != nil {
		if errors.Is(err, ErrReportNotFound) {
			h.renderStatus(w, http.StatusNotFound, "찾을 수 없는 리포트", "리포트 번호를 확인해 주세요.")
			return
		}
		http.Error(w, "리포트를 재시도하지 못했습니다.", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/reports?result=retried", http.StatusSeeOther)
}

func (h *Handler) handleReportDownload(w http.ResponseWriter, r *http.Request, requestContext RequestContext, reportID string) {
	download, err := h.actions.OpenReport(r.Context(), requestContext, reportID)
	if err != nil {
		if download.Body != nil {
			_ = download.Body.Close()
		}
		if errors.Is(err, ErrReportNotFound) {
			h.renderStatus(w, http.StatusNotFound, "찾을 수 없는 리포트", "리포트 번호를 확인해 주세요.")
			return
		}
		http.Error(w, "리포트를 열지 못했습니다.", http.StatusInternalServerError)
		return
	}
	if download.Body == nil || !safeAttachmentName(download.Name) {
		if download.Body != nil {
			_ = download.Body.Close()
		}
		http.Error(w, "리포트를 열지 못했습니다.", http.StatusInternalServerError)
		return
	}
	defer download.Body.Close()
	disposition := mime.FormatMediaType("attachment", map[string]string{"filename": download.Name})
	if disposition == "" {
		http.Error(w, "리포트를 열지 못했습니다.", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Disposition", disposition)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Cache-Control", "private, no-store")
	http.ServeContent(w, r, download.Name, download.Modified, download.Body)
}

func parseDeliveryDays(r *http.Request) ([]int, error) {
	if err := r.ParseForm(); err != nil {
		return nil, err
	}
	values := r.PostForm["delivery_days"]
	if len(values) == 0 {
		return nil, errors.New("no delivery day")
	}
	days := make([]int, 0, len(values))
	seen := make(map[int]bool, len(values))
	for _, value := range values {
		day, err := strconv.Atoi(value)
		if err != nil || day < 0 || day > 6 {
			return nil, errors.New("invalid delivery day")
		}
		if !seen[day] {
			seen[day] = true
			days = append(days, day)
		}
	}
	return days, nil
}

func (h *Handler) handleAddRecipient(w http.ResponseWriter, r *http.Request, requestContext RequestContext) {
	if !validCSRF(r, requestContext) {
		http.Error(w, "요청을 확인할 수 없습니다.", http.StatusForbidden)
		return
	}
	command := RecipientCommand{Name: strings.TrimSpace(r.FormValue("name")), Email: strings.ToLower(strings.TrimSpace(r.FormValue("email")))}
	address, err := mail.ParseAddress(command.Email)
	if command.Name == "" || err != nil || address.Address != command.Email {
		http.Error(w, "수신자 이름과 이메일을 확인해 주세요.", http.StatusBadRequest)
		return
	}
	if err := h.actions.AddRecipient(r.Context(), requestContext, command); err != nil {
		http.Error(w, "수신자를 추가하지 못했습니다.", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/notifications?saved=1", http.StatusSeeOther)
}

func (h *Handler) handlePlatformAction(w http.ResponseWriter, r *http.Request, requestContext RequestContext, action string) {
	if !canViewAdmin(requestContext) {
		http.Error(w, "플랫폼 관리자 권한이 필요합니다.", http.StatusForbidden)
		return
	}
	if !validCSRF(r, requestContext) {
		http.Error(w, "요청을 확인할 수 없습니다.", http.StatusForbidden)
		return
	}
	var err error
	switch action {
	case "collection":
		err = h.actions.RunCollection(r.Context(), requestContext)
	case "test-mail":
		err = h.actions.SendTestMail(r.Context(), requestContext)
	default:
		http.NotFound(w, r)
		return
	}
	if err != nil {
		http.Error(w, "플랫폼 작업을 실행하지 못했습니다.", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/admin?result="+action, http.StatusSeeOther)
}

func (h *Handler) handleCreateTenant(w http.ResponseWriter, r *http.Request, requestContext RequestContext) {
	if !canViewAdmin(requestContext) {
		http.Error(w, "플랫폼 관리자 권한이 필요합니다.", http.StatusForbidden)
		return
	}
	if !validCSRF(r, requestContext) {
		http.Error(w, "요청을 확인할 수 없습니다.", http.StatusForbidden)
		return
	}
	command := TenantCommand{
		Name:      strings.TrimSpace(r.FormValue("tenant_name")),
		AdminName: strings.TrimSpace(r.FormValue("admin_name")),
	}
	contactEmail, contactErr := plainEmail(r.FormValue("contact_email"))
	adminEmail, adminErr := plainEmail(r.FormValue("admin_email"))
	command.ContactEmail, command.AdminEmail = contactEmail, adminEmail
	if command.Name == "" || command.AdminName == "" || contactErr != nil || adminErr != nil ||
		utf8.RuneCountInString(command.Name) > 128 || utf8.RuneCountInString(command.AdminName) > 128 {
		http.Error(w, "회사명, 대표 이메일, 관리자 이름, 관리자 이메일을 확인해 주세요.", http.StatusBadRequest)
		return
	}
	if err := h.actions.CreateTenant(r.Context(), requestContext, command); err != nil {
		if errors.Is(err, ErrTenantExists) {
			http.Error(w, "같은 회사명과 대표 이메일이 이미 등록되어 있습니다.", http.StatusConflict)
			return
		}
		http.Error(w, "회사를 등록하지 못했습니다.", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/admin?result=tenant-created", http.StatusSeeOther)
}

func (h *Handler) handleAssignAccount(w http.ResponseWriter, r *http.Request, requestContext RequestContext) {
	if !canViewAdmin(requestContext) {
		http.Error(w, "플랫폼 관리자 권한이 필요합니다.", http.StatusForbidden)
		return
	}
	if !validCSRF(r, requestContext) {
		http.Error(w, "요청을 확인할 수 없습니다.", http.StatusForbidden)
		return
	}
	command := AssignAccountCommand{
		UserID:   strings.TrimSpace(r.FormValue("user_id")),
		TenantID: strings.TrimSpace(r.FormValue("tenant_id")),
		Role:     strings.TrimSpace(r.FormValue("role")),
	}
	if command.Role == "" {
		command.Role = "member"
	}
	if r.FormValue("mode") == "revoke" {
		command.TenantID, command.Role = "", "member"
	}
	if command.UserID == "" || !allowedValue(command.Role, "member", "tenant_admin") {
		http.Error(w, "대상 계정과 권한을 확인해 주세요.", http.StatusBadRequest)
		return
	}
	if command.TenantID == "" && command.Role != "member" {
		http.Error(w, "회사 관리자 권한은 회사를 함께 선택해야 합니다.", http.StatusBadRequest)
		return
	}
	if err := h.actions.AssignAccountTenant(r.Context(), requestContext, command); err != nil {
		if errors.Is(err, ErrAccountRole) {
			http.Error(w, "회사 관리자 권한은 회사를 함께 선택해야 합니다.", http.StatusBadRequest)
			return
		}
		http.Error(w, "계정 배정을 반영하지 못했습니다.", http.StatusInternalServerError)
		return
	}
	result := "account-assigned"
	if command.TenantID == "" {
		result = "account-revoked"
	}
	http.Redirect(w, r, "/admin?result="+result, http.StatusSeeOther)
}

func (h *Handler) handleRemoveMember(w http.ResponseWriter, r *http.Request, requestContext RequestContext) {
	if !validCSRF(r, requestContext) {
		http.Error(w, "요청을 확인할 수 없습니다.", http.StatusForbidden)
		return
	}
	command := AccountCommand{UserID: strings.TrimSpace(r.FormValue("user_id"))}
	if command.UserID == "" {
		http.Error(w, "대상 구성원을 확인해 주세요.", http.StatusBadRequest)
		return
	}
	if command.UserID == requestContext.UserID {
		http.Error(w, "자신은 회사에서 제외할 수 없습니다.", http.StatusBadRequest)
		return
	}
	if err := h.actions.RemoveMember(r.Context(), requestContext, command); err != nil {
		http.Error(w, "구성원을 제외하지 못했습니다.", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/settings?result=member-removed", http.StatusSeeOther)
}

func (h *Handler) handleDeleteAccount(w http.ResponseWriter, r *http.Request, requestContext RequestContext) {
	if !canViewAdmin(requestContext) {
		http.Error(w, "플랫폼 관리자 권한이 필요합니다.", http.StatusForbidden)
		return
	}
	if !validCSRF(r, requestContext) {
		http.Error(w, "요청을 확인할 수 없습니다.", http.StatusForbidden)
		return
	}
	command := AccountCommand{UserID: strings.TrimSpace(r.FormValue("user_id"))}
	if command.UserID == "" || command.UserID == requestContext.UserID {
		http.Error(w, "삭제할 계정을 확인해 주세요.", http.StatusBadRequest)
		return
	}
	if err := h.actions.DeleteAccount(r.Context(), requestContext, command); err != nil {
		http.Error(w, "계정을 삭제하지 못했습니다.", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/admin?result=account-deleted", http.StatusSeeOther)
}

// renderPending shows the waiting screen for an account without a tenant.
func (h *Handler) renderPending(w http.ResponseWriter, r *http.Request, requestContext RequestContext) {
	if !allows(r.Method, http.MethodGet, http.MethodHead) {
		http.Error(w, "테넌트 배정이 끝난 뒤에 이용할 수 있습니다.", http.StatusForbidden)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	data := page("배정 대기", "", requestContext, false, false)
	h.render(w, "pending", data)
}

func (h *Handler) handleSaveSettings(w http.ResponseWriter, r *http.Request, requestContext RequestContext) {
	if !validCSRF(r, requestContext) {
		http.Error(w, "요청을 확인할 수 없습니다.", http.StatusForbidden)
		return
	}
	command := SettingsCommand{
		TenantName:   strings.TrimSpace(r.FormValue("tenant_name")),
		ContactEmail: strings.TrimSpace(r.FormValue("contact_email")),
	}
	address, err := mail.ParseAddress(command.ContactEmail)
	if command.TenantName == "" || err != nil || address.Address != command.ContactEmail {
		http.Error(w, "회사명과 대표 이메일을 확인해 주세요.", http.StatusBadRequest)
		return
	}
	if err := h.actions.SaveSettings(r.Context(), requestContext, command); err != nil {
		http.Error(w, "환경 설정을 저장하지 못했습니다.", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/settings?saved=1", http.StatusSeeOther)
}

func (h *Handler) handleAcceptInvitation(w http.ResponseWriter, r *http.Request, requestContext RequestContext) {
	if !validCSRF(r, requestContext) {
		http.Error(w, "요청을 확인할 수 없습니다.", http.StatusForbidden)
		return
	}
	command := AcceptInviteCommand{
		Token: strings.TrimSpace(r.FormValue("token")), DisplayName: strings.TrimSpace(r.FormValue("display_name")), Password: r.FormValue("password"),
	}
	passwordBytes := len([]byte(command.Password))
	if command.Token == "" || command.DisplayName == "" || !utf8.ValidString(command.Password) || passwordBytes < 12 || passwordBytes > 72 || command.Password != r.FormValue("password_confirm") {
		http.Error(w, "이름과 12~72바이트 비밀번호를 확인해 주세요.", http.StatusBadRequest)
		return
	}
	if err := h.onboarding.AcceptInvitation(r.Context(), command); err != nil {
		if errors.Is(err, ErrInvitationUnavailable) {
			http.Error(w, "초대가 만료되었거나 이미 사용되었습니다.", http.StatusGone)
			return
		}
		http.Error(w, "계정을 만들지 못했습니다.", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/login?accepted=1", http.StatusSeeOther)
}

func (h *Handler) handleInspectInvitation(w http.ResponseWriter, r *http.Request, requestContext RequestContext) {
	if !validCSRF(r, requestContext) {
		http.Error(w, "요청을 확인할 수 없습니다.", http.StatusForbidden)
		return
	}
	token := strings.TrimSpace(r.FormValue("token"))
	if token == "" {
		http.Error(w, "초대 링크를 확인해 주세요.", http.StatusBadRequest)
		return
	}
	invitation, err := h.onboarding.Invitation(r.Context(), token)
	if err != nil {
		if errors.Is(err, ErrInvitationUnavailable) {
			http.Error(w, "초대가 만료되었거나 이미 사용되었습니다.", http.StatusGone)
			return
		}
		http.Error(w, "초대 정보를 확인하지 못했습니다. 잠시 후 다시 시도해 주세요.", http.StatusServiceUnavailable)
		return
	}
	data := page("초대 수락", "", requestContext, false, false)
	data.Invitation = invitation
	data.InviteToken = token
	data.InviteExpires = invitation.ExpiresAt.In(time.FixedZone("Asia/Seoul", 9*60*60)).Format("2006.01.02 15:04")
	h.render(w, "accept-invite", data)
}

func plainEmail(value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" || strings.ContainsAny(value, "\r\n") {
		return "", errors.New("plain email required")
	}
	address, err := mail.ParseAddress(value)
	if err != nil || !strings.EqualFold(address.Address, value) {
		return "", errors.New("plain email required")
	}
	return strings.ToLower(address.Address), nil
}

func validCSRF(r *http.Request, requestContext RequestContext) bool {
	want := requestContext.CSRFToken
	got := r.FormValue("_csrf")
	return want != "" && got != "" && subtle.ConstantTimeCompare([]byte(got), []byte(want)) == 1
}

func demoNotImplemented(w http.ResponseWriter) {
	http.Error(w, "읽기 전용 데모에서는 변경 내용을 저장하지 않습니다.", http.StatusNotImplemented)
}

func (h *Handler) serveAsset(w http.ResponseWriter, r *http.Request) {
	if strings.HasSuffix(r.URL.Path, ".css") {
		w.Header().Set("Content-Type", "text/css; charset=utf-8")
	}
	if strings.HasSuffix(r.URL.Path, ".js") {
		w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
	}
	w.Header().Set("Cache-Control", "public, max-age=3600")
	h.assets.ServeHTTP(w, withPath(r, strings.TrimPrefix(r.URL.Path, "/assets")))
}

func withPath(r *http.Request, path string) *http.Request {
	clone := r.Clone(r.Context())
	clone.URL.Path = path
	return clone
}

func (h *Handler) render(w http.ResponseWriter, name string, data pageData) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := h.templates.ExecuteTemplate(w, name, data); err != nil {
		http.Error(w, "화면을 표시하지 못했습니다.", http.StatusInternalServerError)
	}
}

func (h *Handler) renderStatus(w http.ResponseWriter, status int, title, message string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	_ = h.templates.ExecuteTemplate(w, "status", struct {
		Status  int
		Title   string
		Message string
	}{status, title, message})
}

func methodNotAllowed(w http.ResponseWriter, methods ...string) {
	w.Header().Set("Allow", strings.Join(methods, ", "))
	http.Error(w, "지원하지 않는 요청 방식입니다.", http.StatusMethodNotAllowed)
}

func allows(method string, methods ...string) bool {
	for _, allowed := range methods {
		if method == allowed {
			return true
		}
	}
	return false
}

func reportRouteID(requestPath, action string) (string, bool) {
	prefix := "/reports/"
	suffix := "/" + action
	if !strings.HasPrefix(requestPath, prefix) || !strings.HasSuffix(requestPath, suffix) {
		return "", false
	}
	id := strings.TrimSuffix(strings.TrimPrefix(requestPath, prefix), suffix)
	if strings.Contains(id, "/") || !validUUID(id) {
		return "", false
	}
	return id, true
}

func validUUID(value string) bool {
	if len(value) != 36 {
		return false
	}
	for index, character := range []byte(value) {
		if index == 8 || index == 13 || index == 18 || index == 23 {
			if character != '-' {
				return false
			}
			continue
		}
		if !((character >= '0' && character <= '9') || (character >= 'a' && character <= 'f') || (character >= 'A' && character <= 'F')) {
			return false
		}
	}
	return true
}

func safeAttachmentName(name string) bool {
	if len(name) == 0 || len(name) > 255 || !utf8.ValidString(name) || !strings.HasSuffix(strings.ToLower(name), ".html") {
		return false
	}
	if !strings.HasPrefix(name, "namo-") {
		if len(name) <= len("20060102_보고서.html") || name[8] != '_' || !strings.HasSuffix(name, "보고서.html") {
			return false
		}
		if _, err := time.Parse("20060102", name[:8]); err != nil {
			return false
		}
	}
	for _, character := range name {
		if unicode.IsControl(character) || strings.ContainsRune(`/\:*?"<>|`, character) {
			return false
		}
	}
	return true
}

func page(title, active string, requestContext RequestContext, writable, demo bool) pageData {
	return pageData{
		Title:       title,
		Active:      active,
		UserName:    requestContext.UserName,
		TenantName:  requestContext.TenantName,
		CurrentDate: time.Now().Format("2006.01.02"),
		Role:        requestContext.Role,
		CSRFToken:   requestContext.CSRFToken,
		Writable:    writable,
		Demo:        demo,
	}
}

func canMutateTenant(requestContext RequestContext) bool {
	if requestContext.TenantID == "" {
		return false
	}
	return requestContext.Role == "tenant_admin" || requestContext.Role == "platform_admin"
}

func canMutateReport(requestContext RequestContext) bool {
	return requestContext.TenantID != "" && requestContext.Role == "tenant_admin"
}

func canViewAdmin(requestContext RequestContext) bool {
	return requestContext.Role == "platform_admin"
}

// removableMembers marks every member except the caller, because a company
// administrator must not remove itself and leave the company unmanaged.
func removableMembers(members []MemberView, requestContext RequestContext) []MemberView {
	if len(members) == 0 {
		return nil
	}
	marked := make([]MemberView, len(members))
	copy(marked, members)
	for index := range marked {
		marked[index].Removable = marked[index].UserID != "" && marked[index].UserID != requestContext.UserID
	}
	return marked
}

func awaitingTenant(requestContext RequestContext) bool {
	return requestContext.UserID != "" && requestContext.TenantID == "" && requestContext.Role != "platform_admin"
}

func safeURL(value string) string {
	parsed, err := url.ParseRequestURI(value)
	if err != nil || parsed.Host == "" || (parsed.Scheme != "https" && parsed.Scheme != "http") {
		return ""
	}
	return value
}

func formatNumber(value int) string {
	digits := strconv.Itoa(value)
	start := 0
	if strings.HasPrefix(digits, "-") {
		start = 1
	}
	for i := len(digits) - 3; i > start; i -= 3 {
		digits = digits[:i] + "," + digits[i:]
	}
	return digits
}

func hasDay(days []int, wanted int) bool {
	for _, day := range days {
		if day == wanted {
			return true
		}
	}
	return false
}

func weekdays() []string {
	return []string{"일", "월", "화", "수", "목", "금", "토"}
}

func state(r *http.Request) string {
	switch r.URL.Query().Get("state") {
	case "empty", "error", "loading":
		return r.URL.Query().Get("state")
	default:
		return "ready"
	}
}

func sampleNotices() []noticeView {
	return []noticeView{
		{
			ID: "2026-sample-001", Title: "샘플: 회계감사 용역", Category: "용역", Agency: "샘플 공공기관",
			Region: "전국", Amount: "120,000,000원", Deadline: "2026.09.08 17:00",
			Reasons:       []string{"포함 키워드 ‘회계감사’ 일치", "예정금액 5천만원 이상"},
			FilterReasons: map[string][]string{"1": {"포함 키워드 ‘회계감사’ 일치", "예정금액 5천만원 이상"}},
		},
		{
			ID: "2026-sample-002", Title: "샘플: 정보시스템 운영 지원", Category: "용역", Agency: "샘플 연구원",
			Region: "서울", Amount: "85,000,000원", Deadline: "2026.09.10 16:00",
			Reasons:       []string{"포함 키워드 ‘운영 지원’ 일치", "지역 ‘서울’ 일치"},
			FilterReasons: map[string][]string{"2": {"포함 키워드 ‘운영 지원’ 일치", "지역 ‘서울’ 일치"}},
		},
		{
			ID: "2026-sample-003", Title: "샘플: 사무용 장비 구매", Category: "물품", Agency: "샘플 재단",
			Region: "경기", Amount: "42,000,000원", Deadline: "2026.09.12 15:00",
			Reasons: []string{"업종 ‘물품’ 일치", "기관 ‘재단’ 일치"},
		},
	}
}

func filterNotices(notices []noticeView, query, filterID, category, region string) []noticeView {
	query = strings.ToLower(query)
	filtered := make([]noticeView, 0, len(notices))
	for _, notice := range notices {
		if filterID != "" {
			reasons, matched := notice.FilterReasons[filterID]
			if !matched {
				continue
			}
			notice.Reasons = reasons
			notice.Keyword = notice.FilterKeywords[filterID]
		}
		searchable := strings.ToLower(notice.Title + " " + notice.Agency)
		if query != "" && !strings.Contains(searchable, query) {
			continue
		}
		if category != "" && notice.Category != category {
			continue
		}
		if !matchesRegionSearch(notice.Region, region) {
			continue
		}
		filtered = append(filtered, notice)
	}
	return filtered
}

func paginateNotices(notices []noticeView, query url.Values) ([]noticeView, paginationView) {
	pageSize := 10
	if requested, err := strconv.Atoi(query.Get("per_page")); err == nil && (requested == 10 || requested == 20 || requested == 30) {
		pageSize = requested
	}
	page := 1
	if requested, err := strconv.Atoi(query.Get("page")); err == nil && requested > 0 {
		page = requested
	}
	pages := (len(notices) + pageSize - 1) / pageSize
	if pages == 0 {
		page = 1
	} else if page > pages {
		page = pages
	}
	start := (page - 1) * pageSize
	if start > len(notices) {
		start = len(notices)
	}
	end := start + pageSize
	if end > len(notices) {
		end = len(notices)
	}
	view := paginationView{Page: page, Pages: pages, PageSize: pageSize, Total: len(notices)}
	if page > 1 {
		view.PreviousURL = noticePageURL(query, page-1, pageSize)
	}
	if page < pages {
		view.NextURL = noticePageURL(query, page+1, pageSize)
	}
	return notices[start:end], view
}

func noticePageURL(query url.Values, page, pageSize int) string {
	values := make(url.Values, len(query))
	for key, items := range query {
		values[key] = append([]string(nil), items...)
	}
	values.Set("page", strconv.Itoa(page))
	values.Set("per_page", strconv.Itoa(pageSize))
	return "/notices?" + values.Encode()
}

func matchesRegionSearch(noticeRegion, query string) bool {
	query = strings.TrimSpace(query)
	if query == "" {
		return true
	}
	noticeRegion = strings.ToLower(noticeRegion)
	for _, term := range strings.Split(query, ",") {
		term = strings.ToLower(strings.TrimSpace(term))
		if term != "" && strings.Contains(noticeRegion, term) {
			return true
		}
	}
	return false
}

func findNotice(notices []noticeView, id string) (noticeView, bool) {
	if id == "" || strings.Contains(id, "/") {
		return noticeView{}, false
	}
	for _, notice := range notices {
		if notice.ID == id {
			return notice, true
		}
	}
	return noticeView{}, false
}

func firstReason(reasons []string) string {
	if len(reasons) == 0 {
		return ""
	}
	return reasons[0]
}

func initial(name string) string {
	for _, r := range name {
		return string(r)
	}
	return ""
}

func sampleFilters() []filterView {
	return []filterView{
		{"1", "회계감사 용역", "포함: 회계감사 · 제외: 상주 · 전국", 7, true},
		{"2", "IT 운영 지원", "포함: 운영 지원 · 서울/경기 · 5천만원 이상", 12, true},
		{"3", "사무 장비", "물품 · 기관명에 재단 포함", 3, false},
	}
}

func sampleRecipients() []recipientView {
	return []recipientView{
		{"김담당", "manager@example.com", "수신"},
		{"이관리", "admin@example.com", "수신"},
	}
}

func sampleMembers() []memberView {
	return []memberView{
		{UserID: "1f0a1f0a-0001-4a00-8000-000000000001", Name: "김담당", Email: "manager@example.com", Role: "일반 사용자", Removable: true},
		{UserID: "1f0a1f0a-0002-4a00-8000-000000000002", Name: "이관리", Email: "admin@example.com", Role: "회사 관리자"},
	}
}

func sampleTenants() []tenantView {
	return []tenantView{
		{Name: "샘플 주식회사", Members: 2, LastDigest: "오늘 07:00", State: "정상", AdminName: "김담당", AdminEmail: "admin@example.com", ContactMail: "contact@example.com"},
		{Name: "테스트 협력사", Members: 1, LastDigest: "생성 전", State: "점검", AdminName: "이담당", AdminEmail: "partner@example.com", ContactMail: "partner@example.com"},
	}
}

func sampleAccounts() []AccountView {
	return []AccountView{
		{UserID: "8f14e45f-ea8f-4b6d-9c1f-6b1f0a1f0001", Email: "newcomer@example.com", DisplayName: "newcomer", Created: "2026.09.03 09:12", Role: "member", RoleLabel: "일반 사용자"},
		{UserID: "8f14e45f-ea8f-4b6d-9c1f-6b1f0a1f0002", Email: "member@example.com", DisplayName: "member", TenantID: "5f6d7e8a-1b2c-4d3e-8f90-a1b2c3d4e5f6", TenantName: "샘플 주식회사", Created: "2026.08.28 14:03", Role: "tenant_admin", RoleLabel: "회사 관리자", Assigned: true},
	}
}

func sampleTenantOptions() []TenantOption {
	return []TenantOption{
		{ID: "5f6d7e8a-1b2c-4d3e-8f90-a1b2c3d4e5f6", Name: "샘플 주식회사"},
		{ID: "6f7e8d9a-2c3b-4e5d-9f80-b2c3d4e5f6a7", Name: "테스트 협력사"},
	}
}

type sampleBackend struct{}

func (sampleBackend) Load(context.Context, RequestContext, PageRequest) (AppData, error) {
	return AppData{
		Dashboard: DashboardView{
			LastCollected: "오늘 06:12",
			Collected:     1284,
			NewNotices:    22,
			Matches:       7,
			ActiveFilters: 2,
			RunTime:       "07:00",
			NextDelivery:  "내일 07:00",
			Healthy:       true,
		},
		Notices:    sampleNotices(),
		Filters:    sampleFilters(),
		Recipients: sampleRecipients(),
		Reports: []ReportView{
			{ID: "123e4567-e89b-12d3-a456-426614174000", FileName: "namo-20260902-070000.html", Trigger: "예약", Status: "생성 완료", DueAt: "2026.09.02 07:00", GeneratedAt: "2026.09.02 07:01", NoticeCount: 7, Downloadable: true},
			{ID: "223e4567-e89b-12d3-a456-426614174000", Trigger: "예약", Status: "재시도 대기", DueAt: "2026.09.01 07:00", GeneratedAt: "-"},
			{ID: "323e4567-e89b-12d3-a456-426614174000", Trigger: "수동", Status: "생성 실패", DueAt: "2026.08.31 15:20", GeneratedAt: "-"},
		},
		Members:       sampleMembers(),
		Tenants:       sampleTenants(),
		Accounts:      sampleAccounts(),
		TenantOptions: sampleTenantOptions(),
		DeliveryTime:  "07:00",
		DeliveryDays:  []int{1, 2, 3, 4, 5},
		Timezone:      "Asia/Seoul",
		ContactEmail:  "admin@example.com",
		Admin: AdminView{
			Healthy:        true,
			LastCollected:  "오늘 06:12",
			CollectedCount: 1284,
			FailedJobs:     0,
		},
		Demo: true,
	}, nil
}
