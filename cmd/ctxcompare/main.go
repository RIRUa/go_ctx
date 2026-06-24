// Command ctxcompare は「ES が遅いとき、パターン1(派生)とパターン2(独立+伝播)で
// 挙動が変わるのか?」を実測で確かめるためのプログラムです。
//
// 結論: サーバー timeout を子へ伝播させる限り、両者の「いつ打ち切られるか/原因は何か」は
// 同一になります（min(サーバー残予算, ES個別予算) で切れる）。ES が遅いことは
// どちらのパターンでも同じように扱われます。
//
// さらに細部では、パターン1の方が ctx.Deadline() が正確になります（実 ES/HTTP
// クライアントはこの値からソケットの read deadline を決めるため、ここがズレると
// 実害が出ます）。詳細は scenarioSlowES_ServerTight 内のコメント参照。
//
// 実行: go run ./cmd/ctxcompare
package main

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/rirua/go_ctx/internal/esfake"
)

func main() {
	// シナリオA: サーバー予算が短く、ES 個別予算を長め(遅いES想定)に取った場合。
	scenarioSlowES_ServerTight()
	// シナリオB: サーバー予算は十分だが ES 個別予算が短く、ES がそれより遅い場合。
	scenarioSlowES_ESBudgetTight()
}

// runBoth は、同じ (serverBudget, esTimeout, esLatency) を
// パターン1(client.Search) と パターン2(client.SearchWithServerPropagation) の
// 両方に通し、経過時間と原因を並べて表示します。
func runBoth(serverBudget, esTimeout, esLatency time.Duration) {
	fmt.Printf("  条件: サーバー予算=%v / ES個別予算=%v / ES応答latency=%v\n",
		serverBudget, esTimeout, esLatency)

	// --- パターン1: 派生 ---
	{
		serverCtx, cancel := context.WithTimeout(context.Background(), serverBudget)
		client := esfake.New(esTimeout, esLatency)
		start := time.Now()
		_, err := client.Search(serverCtx, "q")
		elapsed := time.Since(start)
		fmt.Printf("    [パターン1 派生 ] 経過=%v 原因=%s サーバー生存=%v\n",
			elapsed.Round(10*time.Millisecond), classify(err), serverCtx.Err() == nil)
		cancel()
	}

	// --- パターン2: 独立 + サーバー伝播 ---
	{
		serverCtx, cancel := context.WithTimeout(context.Background(), serverBudget)
		client := esfake.New(esTimeout, esLatency)
		start := time.Now()
		_, err := client.SearchWithServerPropagation(serverCtx, "q")
		elapsed := time.Since(start)
		fmt.Printf("    [パターン2 独立 ] 経過=%v 原因=%s サーバー生存=%v\n",
			elapsed.Round(10*time.Millisecond), classify(err), serverCtx.Err() == nil)
		cancel()
	}
}

// scenarioSlowES_ServerTight は「ES が遅いから ES 個別予算を長く取る」という、
// ユーザーが想定しているケースです。サーバー予算(100ms)の方が短い。
func scenarioSlowES_ServerTight() {
	section("シナリオA: ES個別予算を長め(=遅いES想定)、サーバー予算が短い")
	// ES に 500ms 与えたつもりでも、サーバーは 100ms で応答を返さねばならない。
	runBoth(100*time.Millisecond, 500*time.Millisecond, 300*time.Millisecond)
	fmt.Println("  => 両パターンとも ~100ms でサーバー由来で打ち切り。結果は同一。")
	fmt.Println("     『ES個別予算を 500ms に伸ばした』効果は両者とも出ない(サーバー予算で頭打ち)。")
	fmt.Println()
	fmt.Println("     細部の違い: このとき ctx.Deadline() が指す残り時間は…")
	fmt.Println("       パターン1(派生) = 100ms (サーバー側に丸められ正確)")
	fmt.Println("       パターン2(独立) = 500ms (ES個別予算のまま。実際は100msで殺されるのにズレる)")
	fmt.Println("     実 ES/HTTP クライアントは ctx.Deadline() からソケットの read deadline を")
	fmt.Println("     決めるため、パターン2のズレは『I/O deadline を過大に設定してしまう』実害になりうる。")
}

// scenarioSlowES_ESBudgetTight は、サーバー予算は十分だが ES 個別予算(100ms)より
// ES が遅い(300ms)ケース。ES だけ打ち切ってサーバーは生かしたい状況。
func scenarioSlowES_ESBudgetTight() {
	section("シナリオB: サーバー予算は十分、ES個別予算より ES が遅い")
	runBoth(500*time.Millisecond, 100*time.Millisecond, 300*time.Millisecond)
	fmt.Println("  => 両パターンとも ~100ms で ES個別予算超過。サーバーは生存。結果は同一。")
}

func classify(err error) string {
	switch {
	case err == nil:
		return "成功"
	case errors.Is(err, esfake.ErrBudgetExceeded):
		return "ES個別予算超過"
	case errors.Is(err, context.DeadlineExceeded):
		return "サーバー由来(DeadlineExceeded)"
	case errors.Is(err, context.Canceled):
		return "Canceled"
	default:
		return fmt.Sprintf("その他(%v)", err)
	}
}

func section(title string) {
	fmt.Printf("\n==================== %s ====================\n", title)
}
