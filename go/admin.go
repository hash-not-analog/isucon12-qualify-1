package isuports

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/go-sql-driver/mysql"
	"github.com/labstack/echo/v4"
)

type TenantsAddHandlerResult struct {
	Tenant TenantWithBilling `json:"tenant"`
}

// SasS管理者用API
// テナントを追加する
// POST /api/admin/tenants/add
func tenantsAddHandler(c echo.Context) error {
	v, err := parseViewer(c)
	if err != nil {
		return fmt.Errorf("error parseViewer: %w", err)
	}
	if v.tenantName != "admin" {
		// admin: SaaS管理者用の特別なテナント名
		return echo.NewHTTPError(
			http.StatusNotFound,
			fmt.Sprintf("%s has not this API", v.tenantName),
		)
	}
	if v.role != RoleAdmin {
		return echo.NewHTTPError(http.StatusForbidden, "admin role required")
	}

	displayName := c.FormValue("display_name")
	name := c.FormValue("name")
	if err := validateTenantName(name); err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, err.Error())
	}

	ctx := context.Background()
	now := time.Now().Unix()
	insertRes, err := adminDB.ExecContext(
		ctx,
		"INSERT INTO tenant (name, display_name, created_at, updated_at) VALUES (?, ?, ?, ?)",
		name, displayName, now, now,
	)
	if err != nil {
		if merr, ok := err.(*mysql.MySQLError); ok && merr.Number == 1062 { // duplicate entry
			return echo.NewHTTPError(http.StatusBadRequest, "duplicate tenant")
		}
		return fmt.Errorf(
			"error Insert tenant: name=%s, displayName=%s, createdAt=%d, updatedAt=%d, %w",
			name, displayName, now, now, err,
		)
	}

	id, err := insertRes.LastInsertId()
	if err != nil {
		return fmt.Errorf("error get LastInsertId: %w", err)
	}
	// NOTE: 先にadminDBに書き込まれることでこのAPIの処理中に
	//       /api/admin/tenants/billingにアクセスされるとエラーになりそう
	//       ロックなどで対処したほうが良さそう
	if err := createTenantDB(id); err != nil {
		return fmt.Errorf("error createTenantDB: id=%d name=%s %w", id, name, err)
	}

	res := TenantsAddHandlerResult{
		Tenant: TenantWithBilling{
			ID:          strconv.FormatInt(id, 10),
			Name:        name,
			DisplayName: displayName,
			BillingYen:  0,
		},
	}
	return c.JSON(http.StatusOK, SuccessResult{Status: true, Data: res})
}

// テナント名が規則に沿っているかチェックする
func validateTenantName(name string) error {
	if tenantNameRegexp.MatchString(name) {
		return nil
	}
	return fmt.Errorf("invalid tenant name: %s", name)
}

type TenantWithBilling struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	DisplayName string `json:"display_name"`
	BillingYen  int64  `json:"billing"`
	tenantID    int64  `json:"-"`
}

type TenantsBillingHandlerResult struct {
	Tenants []TenantWithBilling `json:"tenants"`
}

type ScoredPlayer struct {
	ID            string `db:"pid"`
	CompetitionID string `db:"competition_id"`
}

// SaaS管理者用API
// テナントごとの課金レポートを最大10件、テナントのid降順で取得する
// GET /api/admin/tenants/billing
// URL引数beforeを指定した場合、指定した値よりもidが小さいテナントの課金レポートを取得する
// func tenantsBillingHandler(c echo.Context) error {
// 	if host := c.Request().Host; host != getEnv("ISUCON_ADMIN_HOSTNAME", "admin.t.isucon.dev") {
// 		return echo.NewHTTPError(
// 			http.StatusNotFound,
// 			fmt.Sprintf("invalid hostname %s", host),
// 		)
// 	}

// 	ctx := context.Background()
// 	if v, err := parseViewer(c); err != nil {
// 		return err
// 	} else if v.role != RoleAdmin {
// 		return echo.NewHTTPError(http.StatusForbidden, "admin role required")
// 	}

// 	before := c.QueryParam("before")
// 	var beforeID int64
// 	if before != "" {
// 		var err error
// 		beforeID, err = strconv.ParseInt(before, 10, 64)
// 		if err != nil {
// 			return echo.NewHTTPError(
// 				http.StatusBadRequest,
// 				fmt.Sprintf("failed to parse query parameter 'before': %s", err.Error()),
// 			)
// 		}
// 	}
// 	// テナントごとに
// 	//   大会ごとに
// 	//     scoreが登録されているplayer * 100
// 	//     scoreが登録されていないplayerでアクセスした人 * 10
// 	//   を合計したものを
// 	// テナントの課金とする
// 	// ts := []TenantRow{}
// 	// if err := adminDB.SelectContext(ctx, &ts, "SELECT * FROM tenant ORDER BY id DESC"); err != nil {
// 	// 	return fmt.Errorf("error Select tenant: %w", err)

// 	// player_scoreを読んでいるときに更新が走ると不整合が起こるのでロックを取得する
// 	billingMap := map[string]string{}

// 	tenants := make([]TenantRow, 0, 200)
// 	err := adminDB.SelectContext(c.Request().Context(), &tenants, "SELECT * FROM tenant ORDER BY id DESC") // }
// 	if err != nil {
// 		return fmt.Errorf("error Select tenant: %w", err)
// 	}

// 	tenantBillings := make([]TenantWithBilling, 0, len(tenants))

// 	for i := range tenants {
// 		if beforeID != 0 && beforeID <= tenants[i].ID {
// 			continue
// 		}

// 		tenantBillings = append(tenantBillings, TenantWithBilling{
// 			ID:          strconv.FormatInt(tenants[i].ID, 10),
// 			Name:        tenants[i].Name,
// 			DisplayName: tenants[i].DisplayName,
// 			BillingYen:  0,
// 			tenantID:    tenants[i].ID,
// 		})

// 		if len(tenantBillings) >= 10 {
// 			break
// 		}
// 	}

// 	currentCompID := ""

// 	for i := range tenantBillings {
// 		tenantDB, _ := connectToTenantDB(tenantBillings[i].tenantID)

// 		fl, err := flockByTenantID(tenantBillings[i].tenantID)
// 		if err != nil {
// 			return fmt.Errorf("error flockByTenantID: %w", err)
// 		}

// 		// スコアを登録した参加者のIDを取得する
// 		scoredPlayers := []scoredPlayer{}
// 		if err := tenantDB.SelectContext(
// 			ctx,
// 			&scoredPlayers,
// 			"SELECT DISTINCT(player_id) as pid, competition_id FROM player_score WHERE tenant_id = ? ORDER BY competition_id",
// 			tenantBillings[i].tenantID,
// 		); err != nil && err != sql.ErrNoRows {
// 			return fmt.Errorf("error Select count player_score: %w", err)
// 		}

// 		var comp *CompetitionRow
// 		for j := range scoredPlayers {
// 			if currentCompID != scoredPlayers[j].CompetitionID {
// 				currentCompID = scoredPlayers[j].CompetitionID
// 				comp, _ = retrieveCompetition(ctx, tenantDB, currentCompID)
// 			}

// 			if comp == nil || !comp.FinishedAt.Valid {
// 				continue
// 			}

// 			// スコアが登録されている参加者
// 			billingMap[scoredPlayers[j].ID] = "player"
// 			tenantBillings[i].BillingYen += 100
// 		}

// 		vhs := []VisitHistorySummaryRow{}
// 		if err := adminDB.SelectContext(ctx, &vhs,
// 			"SELECT player_id, MIN(created_at) AS min_created_at, competition_id FROM visit_history "+
// 				"WHERE tenant_id = ? GROUP BY player_id, competition_id ORDER BY competition_id",
// 			tenantBillings[i].tenantID,
// 		); err != nil && err != sql.ErrNoRows {
// 			return fmt.Errorf("error Select visit_history. %w", err)
// 		}
// 		for j := range vhs {
// 			var comp *CompetitionRow
// 			if currentCompID != vhs[j].CompetitionID {
// 				currentCompID = vhs[j].CompetitionID
// 				comp, _ = retrieveCompetition(ctx, tenantDB, currentCompID)
// 			}

// 			if comp == nil || !comp.FinishedAt.Valid {
// 				continue
// 			}

// 			// competition.finished_atよりもあとの場合は、終了後に訪問したとみなして大会開催内アクセス済みとみなさない
// 			if comp.FinishedAt.Int64 < vhs[j].MinCreatedAt {
// 				continue
// 			}

// 			if billingMap[vhs[j].PlayerID] != "player" {
// 				tenantBillings[i].BillingYen += 10
// 			}
// 		}

// 		fl.Close()
// 	}
// 	return c.JSON(http.StatusOK, SuccessResult{
// 		Status: true,
// 		Data: TenantsBillingHandlerResult{
// 			Tenants: tenantBillings,
// 		},
// 	})
// }

func tenantsBillingHandler(c echo.Context) error {
	if host := c.Request().Host; host != getEnv("ISUCON_ADMIN_HOSTNAME", "admin.t.isucon.dev") {
		return echo.NewHTTPError(
			http.StatusNotFound,
			fmt.Sprintf("invalid hostname %s", host),
		)
	}

	ctx := context.Background()
	if v, err := parseViewer(c); err != nil {
		return err
	} else if v.role != RoleAdmin {
		return echo.NewHTTPError(http.StatusForbidden, "admin role required")
	}

	before := c.QueryParam("before")
	var beforeID int64
	if before != "" {
		var err error
		beforeID, err = strconv.ParseInt(before, 10, 64)
		if err != nil {
			return echo.NewHTTPError(
				http.StatusBadRequest,
				fmt.Sprintf("failed to parse query parameter 'before': %s", err.Error()),
			)
		}
	}
	// テナントごとに
	//   大会ごとに
	//     scoreが登録されているplayer * 100
	//     scoreが登録されていないplayerでアクセスした人 * 10
	//   を合計したものを
	// テナントの課金とする
	ts := []TenantRow{}
	if err := adminDB.SelectContext(ctx, &ts, "SELECT * FROM tenant ORDER BY id DESC"); err != nil {
		return fmt.Errorf("error Select tenant: %w", err)
	}
	tenantBillings := make([]TenantWithBilling, 0, len(ts))
	for _, t := range ts {
		if beforeID != 0 && beforeID <= t.ID {
			continue
		}
		err := func(t TenantRow) error {
			tb := TenantWithBilling{
				ID:          strconv.FormatInt(t.ID, 10),
				Name:        t.Name,
				DisplayName: t.DisplayName,
			}
			tenantDB, err := connectToTenantDB(t.ID)
			if err != nil {
				return fmt.Errorf("failed to connectToTenantDB: %w", err)
			}
			cs := []CompetitionRow{}
			if err := tenantDB.SelectContext(
				ctx,
				&cs,
				"SELECT * FROM competition WHERE tenant_id=?",
				t.ID,
			); err != nil {
				return fmt.Errorf("failed to Select competition: %w", err)
			}
			for _, comp := range cs {
				report, err := billingReportByCompetition(ctx, tenantDB, t.ID, comp.ID)
				if err != nil {
					return fmt.Errorf("failed to billingReportByCompetition: %w", err)
				}
				tb.BillingYen += report.BillingYen
			}
			tenantBillings = append(tenantBillings, tb)
			return nil
		}(t)
		if err != nil {
			return err
		}
		if len(tenantBillings) >= 10 {
			break
		}
	}

	vhsCache.Reset()

	return c.JSON(http.StatusOK, SuccessResult{
		Status: true,
		Data: TenantsBillingHandlerResult{
			Tenants: tenantBillings,
		},
	})
}
