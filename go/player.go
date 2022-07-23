package isuports

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"time"

	"github.com/labstack/echo/v4"
	"github.com/logica0419/helpisu"
)

var (
	visitHistories = helpisu.NewCache[int, []VisitHistoryRow]()
)

type PlayerScoreDetail struct {
	CompetitionTitle string `json:"competition_title"`
	Score            int64  `json:"score"`
}

type PlayerHandlerResult struct {
	Player PlayerDetail        `json:"player"`
	Scores []PlayerScoreDetail `json:"scores"`
}

// 参加者向けAPI
// GET /api/player/player/:player_id
// 参加者の詳細情報を取得する
func playerHandler(c echo.Context) error {
	ctx := context.Background()

	v, err := parseViewer(c)
	if err != nil {
		return err
	}
	if v.role != RolePlayer {
		return echo.NewHTTPError(http.StatusForbidden, "role player required")
	}

	tenantDB, err := connectToTenantDB(v.tenantID)
	if err != nil {
		return err
	}

	if err := authorizePlayer(ctx, tenantDB, v.playerID); err != nil {
		return err
	}

	playerID := c.Param("player_id")
	if playerID == "" {
		return echo.NewHTTPError(http.StatusBadRequest, "player_id is required")
	}
	p, err := retrievePlayer(ctx, tenantDB, playerID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return echo.NewHTTPError(http.StatusNotFound, "player not found")
		}
		return fmt.Errorf("error retrievePlayer: %w", err)
	}
	// cs := []CompetitionRow{}
	// if err := tenantDB.SelectContext(
	// 	ctx,
	// 	&cs,
	// 	"SELECT * FROM competition WHERE tenant_id = ? ORDER BY created_at ASC",
	// 	v.tenantID,
	// ); err != nil && !errors.Is(err, sql.ErrNoRows) {
	// 	return fmt.Errorf("error Select competition: %w", err)
	// }

	type Row struct {
		Score  int64  `db:"score"`
		Title  string `db:"title"`
		CompID string `db:"comp_id"`
	}

	// player_scoreを読んでいるときに更新が走ると不整合が起こるのでロックを取得する
	fl, err := flockByTenantID(v.tenantID)
	if err != nil {
		return fmt.Errorf("error flockByTenantID: %w", err)
	}
	defer fl.Close()
	pss := make([]Row, 0, 10000)
	// for _, c := range cs {
	// ps := PlayerScoreRow{}
	if err := tenantDB.SelectContext(
		ctx,
		&pss,
		// 最後にCSVに登場したスコアを採用する = row_numが一番大きいもの
		"SELECT player_score.score AS score, competition.title AS title, competition.id as comp_id "+
			"FROM player_score JOIN competition ON competition.id = player_score.competition_id "+
			"WHERE player_score.tenant_id = ? AND player_score.player_id = ? "+
			"ORDER BY competition.created_at ASC, player_score.competition_id ASC, player_score.row_num DESC",
		v.tenantID,
		p.ID,
	); err != nil {
		// // 行がない = スコアが記録されてない
		// if errors.Is(err, sql.ErrNoRows) {
		// 	continue
		// }
		return fmt.Errorf("error Select player_score: tenantID=%d, playerID=%s, %w", v.tenantID, p.ID, err)
	}
	// pss = append(pss, ps)
	// }

	psds := make([]PlayerScoreDetail, 0, len(pss))
	curCompID := ""
	for _, ps := range pss {
		// comp, err := retrieveCompetition(ctx, tenantDB, ps.CompetitionID)
		// if err != nil {
		// 	return fmt.Errorf("error retrieveCompetition: %w", err)
		// }
		if ps.CompID != curCompID {
			curCompID = ps.CompID

			psds = append(psds, PlayerScoreDetail{
				CompetitionTitle: ps.Title,
				Score:            ps.Score,
			})
		}
	}

	res := SuccessResult{
		Status: true,
		Data: PlayerHandlerResult{
			Player: PlayerDetail{
				ID:             p.ID,
				DisplayName:    p.DisplayName,
				IsDisqualified: p.IsDisqualified,
			},
			Scores: psds,
		},
	}
	return c.JSON(http.StatusOK, res)
}

type CompetitionRank struct {
	Rank              int64  `json:"rank"`
	Score             int64  `json:"score"`
	PlayerID          string `json:"player_id"`
	PlayerDisplayName string `json:"player_display_name"`
	RowNum            int64  `json:"-"` // APIレスポンスのJSONには含まれない
}

type CompetitionRankingHandlerResult struct {
	Competition CompetitionDetail `json:"competition"`
	Ranks       []CompetitionRank `json:"ranks"`
}

var tenantCache = helpisu.NewCache[int64, struct{}]()

// 参加者向けAPI
// GET /api/player/competition/:competition_id/ranking
// 大会ごとのランキングを取得する
func competitionRankingHandler(c echo.Context) error {
	ctx := context.Background()
	v, err := parseViewer(c)
	if err != nil {
		return err
	}
	if v.role != RolePlayer {
		return echo.NewHTTPError(http.StatusForbidden, "role player required")
	}

	tenantDB, err := connectToTenantDB(v.tenantID)
	if err != nil {
		return err
	}

	if err := authorizePlayer(ctx, tenantDB, v.playerID); err != nil {
		return err
	}

	competitionID := c.Param("competition_id")
	if competitionID == "" {
		return echo.NewHTTPError(http.StatusBadRequest, "competition_id is required")
	}

	// 大会の存在確認
	competition, err := retrieveCompetition(ctx, tenantDB, competitionID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return echo.NewHTTPError(http.StatusNotFound, "competition not found")
		}
		return fmt.Errorf("error retrieveCompetition: %w", err)
	}

	now := time.Now().Unix()
	var tenant TenantRow
	_, ok := tenantCache.Get(v.tenantID)
	if !ok {
		if err := adminDB.GetContext(ctx, &tenant, "SELECT id FROM tenant WHERE id = ?", v.tenantID); err != nil {
			return fmt.Errorf("error Select tenant: id=%d, %w", v.tenantID, err)
		}
	} else {
		tenant.ID = v.tenantID
	}

	visitHistory, _ := visitHistories.Get(0)
	visitHistory = append(visitHistory, VisitHistoryRow{v.playerID, tenant.ID, competitionID, now, now})
	visitHistories.Set(0, visitHistory)

	var rankAfter int64
	rankAfterStr := c.QueryParam("rank_after")
	if rankAfterStr != "" {
		if rankAfter, err = strconv.ParseInt(rankAfterStr, 10, 64); err != nil {
			return fmt.Errorf("error strconv.ParseUint: rankAfterStr=%s, %w", rankAfterStr, err)
		}
	}

	// player_scoreを読んでいるときに更新が走ると不整合が起こるのでロックを取得する
	fl, err := flockByTenantID(v.tenantID)
	if err != nil {
		return fmt.Errorf("error flockByTenantID: %w", err)
	}
	defer fl.Close()
	pss := []PlayerScoreRow{}
	if err := tenantDB.SelectContext(
		ctx,
		&pss,
		"SELECT * FROM player_score WHERE tenant_id = ? AND competition_id = ? ORDER BY row_num DESC",
		tenant.ID,
		competitionID,
	); err != nil {
		return fmt.Errorf("error Select player_score: tenantID=%d, competitionID=%s, %w", tenant.ID, competitionID, err)
	}
	ranks := make([]CompetitionRank, 0, len(pss))
	scoredPlayerSet := make(map[string]struct{}, len(pss))
	for _, ps := range pss {
		// player_scoreが同一player_id内ではrow_numの降順でソートされているので
		// 現れたのが2回目以降のplayer_idはより大きいrow_numでスコアが出ているとみなせる
		if _, ok := scoredPlayerSet[ps.PlayerID]; ok {
			continue
		}
		scoredPlayerSet[ps.PlayerID] = struct{}{}
		p, err := retrievePlayer(ctx, tenantDB, ps.PlayerID)
		if err != nil {
			return fmt.Errorf("error retrievePlayer: %w", err)
		}
		ranks = append(ranks, CompetitionRank{
			Score:             ps.Score,
			PlayerID:          p.ID,
			PlayerDisplayName: p.DisplayName,
			RowNum:            ps.RowNum,
		})
	}
	sort.Slice(ranks, func(i, j int) bool {
		if ranks[i].Score == ranks[j].Score {
			return ranks[i].RowNum < ranks[j].RowNum
		}
		return ranks[i].Score > ranks[j].Score
	})
	pagedRanks := make([]CompetitionRank, 0, 100)
	for i, rank := range ranks {
		if int64(i) < rankAfter {
			continue
		}
		pagedRanks = append(pagedRanks, CompetitionRank{
			Rank:              int64(i + 1),
			Score:             rank.Score,
			PlayerID:          rank.PlayerID,
			PlayerDisplayName: rank.PlayerDisplayName,
		})
		if len(pagedRanks) >= 100 {
			break
		}
	}

	res := SuccessResult{
		Status: true,
		Data: CompetitionRankingHandlerResult{
			Competition: CompetitionDetail{
				ID:         competition.ID,
				Title:      competition.Title,
				IsFinished: competition.FinishedAt.Valid,
			},
			Ranks: pagedRanks,
		},
	}
	return c.JSON(http.StatusOK, res)
}

func delayedInsertVisitHistory() {
	visitHistory, _ := visitHistories.Get(0)
	_, _ = adminDB.NamedExec(
		"INSERT INTO visit_history (player_id, tenant_id, competition_id, created_at, updated_at) VALUES (:player_id, :tenant_id, :competition_id, :created_at, :updated_at)",
		visitHistory,
	)
	visitHistory = make([]VisitHistoryRow, 0, 100)
	visitHistories.Set(0, visitHistory)
}

type CompetitionsHandlerResult struct {
	Competitions []CompetitionDetail `json:"competitions"`
}

// 参加者向けAPI
// GET /api/player/competitions
// 大会の一覧を取得する
func playerCompetitionsHandler(c echo.Context) error {
	ctx := context.Background()

	v, err := parseViewer(c)
	if err != nil {
		return err
	}
	if v.role != RolePlayer {
		return echo.NewHTTPError(http.StatusForbidden, "role player required")
	}

	tenantDB, err := connectToTenantDB(v.tenantID)
	if err != nil {
		return err
	}

	if err := authorizePlayer(ctx, tenantDB, v.playerID); err != nil {
		return err
	}
	return competitionsHandler(c, v, tenantDB)
}

// テナント管理者向けAPI
// GET /api/organizer/competitions
// 大会の一覧を取得する
func organizerCompetitionsHandler(c echo.Context) error {
	v, err := parseViewer(c)
	if err != nil {
		return err
	}
	if v.role != RoleOrganizer {
		return echo.NewHTTPError(http.StatusForbidden, "role organizer required")
	}

	tenantDB, err := connectToTenantDB(v.tenantID)
	if err != nil {
		return err
	}

	return competitionsHandler(c, v, tenantDB)
}

func competitionsHandler(c echo.Context, v *Viewer, tenantDB dbOrTx) error {
	ctx := context.Background()

	cs := []CompetitionRow{}
	if err := tenantDB.SelectContext(
		ctx,
		&cs,
		"SELECT * FROM competition WHERE tenant_id=? ORDER BY created_at DESC",
		v.tenantID,
	); err != nil {
		return fmt.Errorf("error Select competition: %w", err)
	}
	cds := make([]CompetitionDetail, 0, len(cs))
	for _, comp := range cs {
		cds = append(cds, CompetitionDetail{
			ID:         comp.ID,
			Title:      comp.Title,
			IsFinished: comp.FinishedAt.Valid,
		})
	}

	res := SuccessResult{
		Status: true,
		Data: CompetitionsHandlerResult{
			Competitions: cds,
		},
	}
	return c.JSON(http.StatusOK, res)
}

type TenantDetail struct {
	Name        string `json:"name"`
	DisplayName string `json:"display_name"`
}

type MeHandlerResult struct {
	Tenant   *TenantDetail `json:"tenant"`
	Me       *PlayerDetail `json:"me"`
	Role     string        `json:"role"`
	LoggedIn bool          `json:"logged_in"`
}
