package isuports

import (
	"context"
	"database/sql"
	"fmt"
	"strconv"

	"github.com/logica0419/helpisu"
)

type BillingReport struct {
	CompetitionID     string `json:"competition_id"`
	CompetitionTitle  string `json:"competition_title"`
	PlayerCount       int64  `json:"player_count"`        // スコアを登録した参加者数
	VisitorCount      int64  `json:"visitor_count"`       // ランキングを閲覧だけした(スコアを登録していない)参加者数
	BillingPlayerYen  int64  `json:"billing_player_yen"`  // 請求金額 スコアを登録した参加者分
	BillingVisitorYen int64  `json:"billing_visitor_yen"` // 請求金額 ランキングを閲覧だけした(スコアを登録していない)参加者分
	BillingYen        int64  `json:"billing_yen"`         // 合計請求金額
}

type VisitHistoryRow struct {
	PlayerID      string `db:"player_id"`
	TenantID      int64  `db:"tenant_id"`
	CompetitionID string `db:"competition_id"`
	CreatedAt     int64  `db:"created_at"`
	UpdatedAt     int64  `db:"updated_at"`
}

type VisitHistorySummaryRow struct {
	PlayerID      string `db:"player_id"`
	MinCreatedAt  int64  `db:"min_created_at"`
	CompetitionID string `db:"competition_id"`
	TenantID      int64  `db:"tenant_id"`
}

var vhsCache = helpisu.NewCache[int64, []VisitHistorySummaryRow]()
var scoredPlayerCache = helpisu.NewCache[int64, []ScoredPlayer]()

var billingReportCache = helpisu.NewCache[string, BillingReport]()

// 大会ごとの課金レポートを計算する
func billingReportByCompetition(ctx context.Context, tenantDB dbOrTx, tenantID int64, competitionID string) (*BillingReport, error) {
	billingReport, ok := billingReportCache.Get(strconv.Itoa(int(tenantID)) + competitionID)
	if ok {
		return &billingReport, nil
	}

	comp, err := retrieveCompetition(ctx, tenantDB, competitionID)
	if err != nil {
		return nil, fmt.Errorf("error retrieveCompetition: %w", err)
	}

	// ランキングにアクセスした参加者のIDを取得する
	vhs, ok := vhsCache.Get(tenantID)
	if !ok {
		if err := adminDB.SelectContext(
			ctx,
			&vhs,
			"SELECT player_id, MIN(created_at) AS min_created_at, competition_id FROM visit_history WHERE tenant_id = ? GROUP BY player_id, competition_id",
			tenantID,
		); err != nil && err != sql.ErrNoRows {
			return nil, fmt.Errorf("error Select visit_history: tenantID=%d, competitionID=%s, %w", tenantID, comp.ID, err)
		}
	}
	billingMap := map[string]string{}
	for i := range vhs {
		if vhs[i].CompetitionID != comp.ID {
			continue
		}

		// competition.finished_atよりもあとの場合は、終了後に訪問したとみなして大会開催内アクセス済みとみなさない
		if comp.FinishedAt.Valid && comp.FinishedAt.Int64 < vhs[i].MinCreatedAt {
			continue
		}
		billingMap[vhs[i].PlayerID] = "visitor"
	}
	vhsCache.Set(tenantID, vhs)

	// player_scoreを読んでいるときに更新が走ると不整合が起こるのでロックを取得する
	fl, err := flockByTenantID(tenantID)
	if err != nil {
		return nil, fmt.Errorf("error flockByTenantID: %w", err)
	}
	defer fl.Close()

	// スコアを登録した参加者のIDを取得する
	scoredPlayers, ok := scoredPlayerCache.Get(tenantID)
	if !ok {
		if err := tenantDB.SelectContext(
			ctx,
			&scoredPlayers,
			"SELECT DISTINCT(player_id) AS pid, competition_id FROM player_score WHERE tenant_id = ?",
			tenantID, comp.ID,
		); err != nil && err != sql.ErrNoRows {
			return nil, fmt.Errorf("error Select count player_score: tenantID=%d, competitionID=%s, %w", tenantID, competitionID, err)
		}
	}
	for i := range scoredPlayers {
		if scoredPlayers[i].CompetitionID != comp.ID {
			continue
		}

		// スコアが登録されている参加者
		billingMap[scoredPlayers[i].ID] = "player"
	}

	// 大会が終了している場合のみ請求金額が確定するので計算する
	var playerCount, visitorCount int64
	if comp.FinishedAt.Valid {
		for _, category := range billingMap {
			switch category {
			case "player":
				playerCount++
			case "visitor":
				visitorCount++
			}
		}
	}

	billingReport = BillingReport{
		CompetitionID:     comp.ID,
		CompetitionTitle:  comp.Title,
		PlayerCount:       playerCount,
		VisitorCount:      visitorCount,
		BillingPlayerYen:  100 * playerCount, // スコアを登録した参加者は100円
		BillingVisitorYen: 10 * visitorCount, // ランキングを閲覧だけした(スコアを登録していない)参加者は10円
		BillingYen:        100*playerCount + 10*visitorCount,
	}

	billingReportCache.Set(strconv.Itoa(int(tenantID))+competitionID, billingReport)

	return &billingReport, nil
}
