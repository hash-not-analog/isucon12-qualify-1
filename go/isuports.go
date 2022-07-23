package isuports

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"net/http"
	_ "net/http/pprof"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/go-sql-driver/mysql"
	"github.com/gofrs/flock"
	"github.com/jmoiron/sqlx"
	"github.com/labstack/echo/v4"
	"github.com/labstack/echo/v4/middleware"
	"github.com/labstack/gommon/log"
	"github.com/lestrrat-go/jwx/v2/jwa"
	"github.com/lestrrat-go/jwx/v2/jwk"
	"github.com/lestrrat-go/jwx/v2/jwt"
	"github.com/logica0419/helpisu"
)

const (
	tenantDBSchemaFilePath = "../sql/tenant/10_schema.sql"
	initializeScript       = "../sql/init.sh"
	cookieName             = "isuports_session"

	RoleAdmin     = "admin"
	RoleOrganizer = "organizer"
	RolePlayer    = "player"
	RoleNone      = "none"
)

var (
	// 正しいテナント名の正規表現
	tenantNameRegexp = regexp.MustCompile(`^[a-z][a-z0-9-]{0,61}[a-z0-9]$`)

	adminDB *sqlx.DB

	sqliteDriverName = "sqlite3"
	tenantDBCache    = helpisu.NewCache[int64, *sqlx.DB]()
	dispenseMu       = sync.Mutex{}
	curId            = int64(-1)
)

// 環境変数を取得する、なければデフォルト値を返す
func getEnv(key string, defaultValue string) string {
	if val, ok := os.LookupEnv(key); ok {
		return val
	}
	return defaultValue
}

// 管理用DBに接続する
func connectAdminDB() (*sqlx.DB, error) {
	config := mysql.NewConfig()
	config.Net = "tcp"
	config.Addr = getEnv("ISUCON_DB_HOST", "127.0.0.1") + ":" + getEnv("ISUCON_DB_PORT", "3306")
	config.User = getEnv("ISUCON_DB_USER", "isucon")
	config.Passwd = getEnv("ISUCON_DB_PASSWORD", "isucon")
	config.DBName = getEnv("ISUCON_DB_NAME", "isuports")
	config.ParseTime = true
	config.InterpolateParams = true
	dsn := config.FormatDSN()
	return sqlx.Open("mysql", dsn)
}

// テナントDBのパスを返す
func tenantDBPath(id int64) string {
	tenantDBDir := getEnv("ISUCON_TENANT_DB_DIR", "../tenant_db")
	return filepath.Join(tenantDBDir, fmt.Sprintf("%d.db", id))
}

// テナントDBに接続する
func connectToTenantDB(id int64) (*sqlx.DB, error) {
	tenantDB, ok := tenantDBCache.Get(id)
	if ok {
		return tenantDB, nil
	}
	p := tenantDBPath(id)
	db, err := sqlx.Open(sqliteDriverName, fmt.Sprintf("file:%s?mode=rw", p))
	if err != nil {
		return nil, fmt.Errorf("failed to open tenant DB: %w", err)
	}
	tenantDBCache.Set(id, db)
	return db, nil
}

// テナントDBを新規に作成する
func createTenantDB(id int64) error {
	if _, ok := tenantDBCache.Get(id); ok {
		return nil
	}

	p := tenantDBPath(id)
	cmd := exec.Command("sh", "-c", fmt.Sprintf("sqlite3 %s < %s", p, tenantDBSchemaFilePath))
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("failed to exec sqlite3 %s < %s, out=%s: %w", p, tenantDBSchemaFilePath, string(out), err)
	}
	return nil
}

// システム全体で一意なIDを生成する
// これMutexと加算で置き換えられる
func dispenseID(ctx context.Context) (string, error) {
	if curId == -1 {
		adminDB.Get(curId, "SELECT id FROM id_generator WHERE stub='a';")
	}
	dispenseMu.Lock()
	curId += 1
	dispenseMu.Unlock()
	return fmt.Sprintf("%x", curId), nil
}

func dispenseUpdate() {
	t := time.NewTicker(90 * time.Second)
	defer t.Stop()
	<-t.C
	adminDB.Exec("UPDATE id_generator SET id = ?, stub=?;", curId, "a")
}

// 全APIにCache-Control: privateを設定する
func SetCacheControlPrivate(next echo.HandlerFunc) echo.HandlerFunc {
	return func(c echo.Context) error {
		c.Response().Header().Set(echo.HeaderCacheControl, "private")
		return next(c)
	}
}

var d *helpisu.DBDisconnectDetector

// Run は cmd/isuports/main.go から呼ばれるエントリーポイントです
func Run() {
	e := echo.New()
	e.Debug = true
	e.Logger.SetLevel(log.DEBUG)

	var (
		sqlLogger io.Closer
		err       error
	)
	// sqliteのクエリログを出力する設定
	// 環境変数 ISUCON_SQLITE_TRACE_FILE を設定すると、そのファイルにクエリログをJSON形式で出力する
	// 未設定なら出力しない
	// sqltrace.go を参照
	sqliteDriverName, sqlLogger, err = initializeSQLLogger()
	if err != nil {
		e.Logger.Panicf("error initializeSQLLogger: %s", err)
	}
	defer sqlLogger.Close()

	e.Use(middleware.Logger())
	e.Use(middleware.Recover())
	e.Use(SetCacheControlPrivate)

	// SaaS管理者向けAPI
	e.POST("/api/admin/tenants/add", tenantsAddHandler)
	e.GET("/api/admin/tenants/billing", tenantsBillingHandler)

	// テナント管理者向けAPI - 参加者追加、一覧、失格
	e.GET("/api/organizer/players", playersListHandler)
	e.POST("/api/organizer/players/add", playersAddHandler)
	e.POST("/api/organizer/player/:player_id/disqualified", playerDisqualifiedHandler)

	// テナント管理者向けAPI - 大会管理
	e.POST("/api/organizer/competitions/add", competitionsAddHandler)
	e.POST("/api/organizer/competition/:competition_id/finish", competitionFinishHandler)
	e.POST("/api/organizer/competition/:competition_id/score", competitionScoreHandler)
	e.GET("/api/organizer/billing", billingHandler)
	e.GET("/api/organizer/competitions", organizerCompetitionsHandler)

	// 参加者向けAPI
	e.GET("/api/player/player/:player_id", playerHandler)
	e.GET("/api/player/competition/:competition_id/ranking", competitionRankingHandler)
	e.GET("/api/player/competitions", playerCompetitionsHandler)

	// 全ロール及び未認証でも使えるhandler
	e.GET("/api/me", meHandler)

	// ベンチマーカー向けAPI
	e.POST("/initialize", initializeHandler)

	e.HTTPErrorHandler = errorResponseHandler

	adminDB, err = connectAdminDB()
	if err != nil {
		e.Logger.Fatalf("failed to connect db: %v", err)
		return
	}
	adminDB.SetMaxOpenConns(10)
	defer adminDB.Close()

	helpisu.WaitDBStartUp(adminDB.DB)

	d = helpisu.NewDBDisconnectDetector(5, 90, adminDB.DB)
	go d.Start()

	// プール内に保持できるアイドル接続数の制限を設定 (default: 2)
	adminDB.SetMaxIdleConns(1024)
	// 接続してから再利用できる最大期間
	adminDB.SetConnMaxLifetime(0)
	// アイドル接続してから再利用できる最大期間
	adminDB.SetConnMaxIdleTime(0)

	http.DefaultTransport.(*http.Transport).MaxIdleConns = 0           // default: 100
	http.DefaultTransport.(*http.Transport).MaxIdleConnsPerHost = 1024 // default: 2
	http.DefaultTransport.(*http.Transport).ForceAttemptHTTP2 = true
	http.DefaultClient.Timeout = 5 * time.Second // 問題の切り分け用

	go http.ListenAndServe(":6060", nil)

	port := getEnv("SERVER_APP_PORT", "3000")
	e.Logger.Infof("starting isuports server on : %s ...", port)
	serverPort := fmt.Sprintf(":%s", port)
	e.Logger.Fatal(e.Start(serverPort))
}

// エラー処理関数
func errorResponseHandler(err error, c echo.Context) {
	c.Logger().Errorf("error at %s: %s", c.Path(), err.Error())
	var he *echo.HTTPError
	if errors.As(err, &he) {
		c.JSON(he.Code, FailureResult{
			Status: false,
		})
		return
	}
	c.JSON(http.StatusInternalServerError, FailureResult{
		Status: false,
	})
}

type SuccessResult struct {
	Status bool `json:"status"`
	Data   any  `json:"data,omitempty"`
}

type FailureResult struct {
	Status  bool   `json:"status"`
	Message string `json:"message"`
}

// アクセスしてきた人の情報
type Viewer struct {
	role       string
	playerID   string
	tenantName string
	tenantID   int64
}

var jwtKeyCache = helpisu.NewCache[bool, any]()

type TokenData struct {
	subject string
	role    string
	aud     []string
}

var jwtTokenCache = helpisu.NewCache[string, TokenData]()

// リクエストヘッダをパースしてViewerを返す
// JWTのキーキャッシュできる
func parseViewer(c echo.Context) (*Viewer, error) {
	cookie, err := c.Request().Cookie(cookieName)
	if err != nil {
		return nil, echo.NewHTTPError(
			http.StatusUnauthorized,
			fmt.Sprintf("cookie %s is not found", cookieName),
		)
	}
	tokenStr := cookie.Value

	var subject, role string
	aud := []string{}
	tokenData, ok := jwtTokenCache.Get(tokenStr)
	if !ok {
		jwtTokenCache.Get(tokenStr)
		key, ok := jwtKeyCache.Get(true)
		if !ok {
			keyFilename := getEnv("ISUCON_JWT_KEY_FILE", "../public.pem")
			keysrc, err := os.ReadFile(keyFilename)
			if err != nil {
				return nil, fmt.Errorf("error os.ReadFile: keyFilename=%s: %w", keyFilename, err)
			}
			key, _, err = jwk.DecodePEM(keysrc)
			if err != nil {
				return nil, fmt.Errorf("error jwk.DecodePEM: %w", err)
			}

			jwtKeyCache.Set(true, key)
		}

		token, err := jwt.Parse(
			[]byte(tokenStr),
			jwt.WithKey(jwa.RS256, key),
		)
		if err != nil {
			return nil, echo.NewHTTPError(http.StatusUnauthorized, fmt.Errorf("error jwt.Parse: %s", err.Error()))
		}
		if subject = token.Subject(); subject == "" {
			return nil, echo.NewHTTPError(
				http.StatusUnauthorized,
				fmt.Sprintf("invalid token: subject is not found in token: %s", tokenStr),
			)
		}

		tr, ok := token.Get("role")
		if !ok {
			return nil, echo.NewHTTPError(
				http.StatusUnauthorized,
				fmt.Sprintf("invalid token: role is not found: %s", tokenStr),
			)
		}
		switch tr {
		case RoleAdmin, RoleOrganizer, RolePlayer:
			role = tr.(string)
		default:
			return nil, echo.NewHTTPError(
				http.StatusUnauthorized,
				fmt.Sprintf("invalid token: invalid role: %s", tokenStr),
			)
		}
		// aud は1要素でテナント名がはいっている
		aud = token.Audience()
		if len(aud) != 1 {
			return nil, echo.NewHTTPError(
				http.StatusUnauthorized,
				fmt.Sprintf("invalid token: aud field is few or too much: %s", tokenStr),
			)
		}

		jwtTokenCache.Set(tokenStr, TokenData{
			subject: subject,
			role:    role,
			aud:     aud,
		})
	} else {
		subject, role, aud = tokenData.subject, tokenData.role, tokenData.aud
	}

	tenant, err := retrieveTenantRowFromHeader(c)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, echo.NewHTTPError(http.StatusUnauthorized, "tenant not found")
		}
		return nil, fmt.Errorf("error retrieveTenantRowFromHeader at parseViewer: %w", err)
	}
	if tenant.Name == "admin" && role != RoleAdmin {
		return nil, echo.NewHTTPError(http.StatusUnauthorized, "tenant not found")
	}

	if tenant.Name != aud[0] {
		return nil, echo.NewHTTPError(
			http.StatusUnauthorized,
			fmt.Sprintf("invalid token: tenant name is not match with %s: %s", c.Request().Host, tokenStr),
		)
	}

	v := &Viewer{
		role:       role,
		playerID:   subject,
		tenantName: tenant.Name,
		tenantID:   tenant.ID,
	}
	return v, nil
}

func retrieveTenantRowFromHeader(c echo.Context) (*TenantRow, error) {
	// JWTに入っているテナント名とHostヘッダのテナント名が一致しているか確認
	baseHost := getEnv("ISUCON_BASE_HOSTNAME", ".t.isucon.dev")
	tenantName := strings.TrimSuffix(c.Request().Host, baseHost)

	// SaaS管理者用ドメイン
	if tenantName == "admin" {
		return &TenantRow{
			Name:        "admin",
			DisplayName: "admin",
		}, nil
	}

	// テナントの存在確認
	var tenant TenantRow
	if err := adminDB.GetContext(
		context.Background(),
		&tenant,
		"SELECT * FROM tenant WHERE name = ?",
		tenantName,
	); err != nil {
		return nil, fmt.Errorf("failed to Select tenant: name=%s, %w", tenantName, err)
	}
	return &tenant, nil
}

type TenantRow struct {
	ID          int64  `db:"id"`
	Name        string `db:"name"`
	DisplayName string `db:"display_name"`
	CreatedAt   int64  `db:"created_at"`
	UpdatedAt   int64  `db:"updated_at"`
}

type dbOrTx interface {
	GetContext(ctx context.Context, dest interface{}, query string, args ...interface{}) error
	SelectContext(ctx context.Context, dest interface{}, query string, args ...interface{}) error
	ExecContext(ctx context.Context, query string, args ...interface{}) (sql.Result, error)
}

type PlayerRow struct {
	TenantID       int64  `db:"tenant_id"`
	ID             string `db:"id"`
	DisplayName    string `db:"display_name"`
	IsDisqualified bool   `db:"is_disqualified"`
	CreatedAt      int64  `db:"created_at"`
	UpdatedAt      int64  `db:"updated_at"`
}

var playerCache = helpisu.NewCache[string, PlayerRow]()

// 参加者を取得する
func retrievePlayer(ctx context.Context, tenantDB dbOrTx, id string) (*PlayerRow, error) {
	p, ok := playerCache.Get(id)
	if !ok {
		if err := tenantDB.GetContext(ctx, &p, "SELECT * FROM player WHERE id = ?", id); err != nil {
			return nil, fmt.Errorf("error Select player: id=%s, %w", id, err)
		}
	}
	playerCache.Set(id, p)
	return &p, nil
}

// 参加者を認可する
// 参加者向けAPIで呼ばれる
func authorizePlayer(ctx context.Context, tenantDB dbOrTx, id string) error {
	player, err := retrievePlayer(ctx, tenantDB, id)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return echo.NewHTTPError(http.StatusUnauthorized, "player not found")
		}
		return fmt.Errorf("error retrievePlayer from viewer: %w", err)
	}
	if player.IsDisqualified {
		return echo.NewHTTPError(http.StatusForbidden, "player is disqualified")
	}
	return nil
}

type CompetitionRow struct {
	TenantID   int64         `db:"tenant_id"`
	ID         string        `db:"id"`
	Title      string        `db:"title"`
	FinishedAt sql.NullInt64 `db:"finished_at"`
	CreatedAt  int64         `db:"created_at"`
	UpdatedAt  int64         `db:"updated_at"`
}

var competitionCache = helpisu.NewCache[string, CompetitionRow]()

// 大会を取得する
func retrieveCompetition(ctx context.Context, tenantDB dbOrTx, id string) (*CompetitionRow, error) {
	c, ok := competitionCache.Get(id)
	if !ok {
		if err := tenantDB.GetContext(ctx, &c, "SELECT * FROM competition WHERE id = ?", id); err != nil {
			return nil, fmt.Errorf("error Select competition: id=%s, %w", id, err)
		}

		competitionCache.Set(id, c)
	}
	return &c, nil
}

type PlayerScoreRow struct {
	TenantID      int64  `db:"tenant_id"`
	ID            string `db:"id"`
	PlayerID      string `db:"player_id"`
	CompetitionID string `db:"competition_id"`
	Score         int64  `db:"score"`
	RowNum        int64  `db:"row_num"`
	CreatedAt     int64  `db:"created_at"`
	UpdatedAt     int64  `db:"updated_at"`
}

// 排他ロックのためのファイル名を生成する
func lockFilePath(id int64) string {
	tenantDBDir := getEnv("ISUCON_TENANT_DB_DIR", "../tenant_db")
	return filepath.Join(tenantDBDir, fmt.Sprintf("%d.lock", id))
}

// 排他ロックする
func flockByTenantID(tenantID int64) (io.Closer, error) {
	p := lockFilePath(tenantID)

	fl := flock.New(p)
	if err := fl.Lock(); err != nil {
		return nil, fmt.Errorf("error flock.Lock: path=%s, %w", p, err)
	}
	return fl, nil
}

type InitializeHandlerResult struct {
	Lang string `json:"lang"`
}

// ベンチマーカー向けAPI
// POST /initialize
// ベンチマーカーが起動したときに最初に呼ぶ
// データベースの初期化などが実行されるため、スキーマを変更した場合などは適宜改変すること
func initializeHandler(c echo.Context) error {
	var tenantNum int
	adminDB.GetContext(c.Request().Context(), &tenantNum, "SELECT count(*) FROM tenant")

	out, err := exec.Command(initializeScript).CombinedOutput()
	if err != nil {
		return fmt.Errorf("error exec.Command: %s %e", string(out), err)
	}

	for i := 1; i < tenantNum; i++ {
		tenantDB, ok := tenantDBCache.Get(int64(i))
		if ok {
			tenantDB.Close()
		}
	}

	tenantDBCache.Reset()
	jwtKeyCache.Reset()
	jwtTokenCache.Reset()
	playerCache.Reset()
	competitionCache.Reset()
	tenantCache.Reset()
	compFinishCache.Reset()
	billingReportCache.Reset()

	go dispenseUpdate()

	visitHistories.Set(0, make([]VisitHistoryRow, 0, 100))
	insertVisitHistory := helpisu.NewTicker(2000, delayedInsertVisitHistory)
	go insertVisitHistory.Start()

	updateCompetitionFinish := helpisu.NewTicker(2000, updateCompetitionFinish)
	go updateCompetitionFinish.Start()

	d.Pause()

	res := InitializeHandlerResult{
		Lang: "go",
	}
	return c.JSON(http.StatusOK, SuccessResult{Status: true, Data: res})
}
