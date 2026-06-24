// Command ctxpatterns は、検討中の 2 つの context 設計パターンを、サーバーや
// Elasticsearch を使わずに動かして比較するための実験プログラムです。
//
// 設計方針: ES 個別の timeout context は「呼び出し側」ではなく「ES クライアント
// (internal/esfake) の中」で作り、cancel もその中で defer します。呼び出し側は
// サーバーのリクエスト context（親）を渡すだけなので、defer cancel の書き忘れによる
// context リークが起きません。
//
// パターン1 (親子・派生):
//
//	esfake.Client.Search が parent から ES 用 context を派生させる。子(ES)の timeout が
//	切れても、親(サーバー)は生かしてフォールバックや部分応答を続けたい、という形。
//
// パターン2 (並列・独立 + 一方向伝播):
//
//	esfake.Client.SearchWithServerPropagation が ES context を Background から独立に作り、
//	サーバー(親)の timeout『だけ』を子(ES)へ一方向に伝播させる形。
//
// 結論の先取り: 実行時のキャンセル挙動はパターン2もパターン1と同じ結果になります。
// 特別な理由が無ければ、自動で親→子伝播が効くパターン1（派生）を勧めます。
//
// 実行: go run ./cmd/ctxpatterns
package main

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/rirua/go_ctx/internal/esfake"
)

func main() {
	pattern1ChildTimesOutParentSurvives()
	pattern1ParentTimesOut()
	pattern2PropagateToChildOnly_ServerTimesOut()
	pattern2PropagateToChildOnly_ESTimesOut()
	pattern2Parallel()
}

// ============================================================
// パターン1: 親子・派生
// ============================================================

// pattern1ChildTimesOutParentSurvives は、子(ES)の timeout が切れても親(サーバー)を
// 生かして後続処理を続ける、という狙い通りに動くことを示します。
//
//	serverCtx (500ms) を client.Search に渡すと、内部で ES 個別 timeout(100ms) を派生。
//	ES 応答は 300ms かかる想定 → ES 個別予算が先に切れる。serverCtx はまだ生きている。
func pattern1ChildTimesOutParentSurvives() {
	section("パターン1: 子(ES)が timeout → 親(サーバー)は生存しフォールバック")

	serverCtx, cancelServer := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancelServer()

	// ES 個別 timeout=100ms / 応答 latency=300ms。
	// 呼び出し側は serverCtx を渡すだけ。esCtx と defer cancel は client 内部。
	client := esfake.New(100*time.Millisecond, 300*time.Millisecond)
	_, err := client.Search(serverCtx, "primary")
	report("ES(primary)", err)

	// パターン1の肝: ES がコケても親 context の状態を見て分岐する。
	if errors.Is(err, esfake.ErrBudgetExceeded) && serverCtx.Err() == nil {
		fmt.Println("    -> ES はタイムアウトしたが serverCtx は生存。フォールバックに進む。")

		// 親 context にはまだ予算が残っているので、軽いフォールバック検索を続けられる。
		fast := esfake.New(100*time.Millisecond, 30*time.Millisecond)
		res, fbErr := fast.Search(serverCtx, "fallback")
		report("ES(fallback)", fbErr)
		fmt.Printf("    クライアントへ返す結果: %q (degraded)\n", res)
	}
	reportCtxState("serverCtx", serverCtx)
	fmt.Println("=> 狙い通り。子の打ち切りは親に伝播しないので、親で安全にハンドリングできる。")
}

// pattern1ParentTimesOut は、親(サーバー)の予算自体が尽きた場合は、派生した子も
// 巻き込まれて止まる（＝リクエスト全体を諦める）ことを示します。
func pattern1ParentTimesOut() {
	section("パターン1: 親(サーバー)の予算が尽きた場合")

	serverCtx, cancelServer := context.WithTimeout(context.Background(), 80*time.Millisecond)
	defer cancelServer()

	// ES 個別には余裕(500ms)を持たせても、親が 80ms で切れれば子も止まる。
	client := esfake.New(500*time.Millisecond, 300*time.Millisecond)
	_, err := client.Search(serverCtx, "primary")
	report("ES(primary)", err)

	if serverCtx.Err() != nil {
		fmt.Printf("    serverCtx も終了済み (%v) なのでフォールバックも不可。クライアントには 503/タイムアウトを返す。\n",
			serverCtx.Err())
	}
	fmt.Println("=> 親が死ねば子も死ぬ（親→子は伝播する）。これは正しい挙動。")
}

// ============================================================
// パターン2: 並列・独立 + 親→子の一方向伝播
// ============================================================

// pattern2PropagateToChildOnly_ServerTimesOut は、ES クライアント内部で独立に作った
// context へ、サーバーの timeout が一方向で伝播し ES が中断されることを示します。
//
//	serverCtx (80ms) を渡す。ES 個別予算は 500ms、応答 latency は 300ms。
//	→ サーバー(80ms)が先に切れ、その伝播で ES も中断される。
func pattern2PropagateToChildOnly_ServerTimesOut() {
	section("パターン2: 独立 esCtx へサーバー timeout が一方向伝播（サーバーが先に切れる）")

	serverCtx, cancelServer := context.WithTimeout(context.Background(), 80*time.Millisecond)
	defer cancelServer()

	client := esfake.New(500*time.Millisecond, 300*time.Millisecond)
	_, err := client.SearchWithServerPropagation(serverCtx, "es-parallel")
	report("ES", err)
	fmt.Println("=> サーバーの timeout が ES に伝播して中断。エラーからサーバー由来と分かる。")
}

// pattern2PropagateToChildOnly_ESTimesOut は、逆に ES 個別予算が先に切れた場合、
// サーバー context は一切影響を受けない（子→親は伝播しない）ことを示します。
func pattern2PropagateToChildOnly_ESTimesOut() {
	section("パターン2: ES 個別予算が先に切れる（サーバーは無傷）")

	serverCtx, cancelServer := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancelServer()

	client := esfake.New(80*time.Millisecond, 300*time.Millisecond)
	_, err := client.SearchWithServerPropagation(serverCtx, "es-parallel")
	report("ES", err)
	reportCtxState("serverCtx", serverCtx)
	fmt.Println("=> ES の打ち切りは独立 root に閉じるのでサーバーは無傷。パターン1と同じ結果。")
}

// pattern2Parallel は「二つ並列でやらせる」イメージそのものを再現します。
//
// メイン goroutine はサーバー本体の処理を進めつつ、ES 呼び出しを別 goroutine で
// 並走させます。サーバー context の timeout は両方に効きます（ES へは内部の AfterFunc
// 経由）。ES が単独でコケてもサーバー側の別処理は止まりません。
func pattern2Parallel() {
	section("パターン2: 二つ並列（ES を別 goroutine で並走、サーバー timeout が両方に効く）")

	serverCtx, cancelServer := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancelServer()

	type esResult struct {
		body string
		err  error
	}
	esDone := make(chan esResult, 1)

	// 並走その1: ES 呼び出し（独立 ctx + サーバー伝播は client 内部に隠蔽）。
	go func() {
		client := esfake.New(150*time.Millisecond, 100*time.Millisecond)
		body, err := client.SearchWithServerPropagation(serverCtx, "parallel-es")
		esDone <- esResult{body, err}
	}()

	// 並走その2: サーバー本体の別処理（ここでは 50ms のダミー処理）。
	time.Sleep(50 * time.Millisecond)
	fmt.Println("    サーバー本体の別処理が完了")

	// ES の結果を、サーバー全体の予算内で待つ。
	select {
	case r := <-esDone:
		report("並走 ES", r.err)
		if r.err == nil {
			fmt.Printf("    両者そろって応答組み立て: es=%q\n", r.body)
		}
	case <-serverCtx.Done():
		fmt.Printf("    サーバー予算切れ (%v) → ES 結果を待たずに応答\n", context.Cause(serverCtx))
	}
	fmt.Println("=> 並列でもサーバー timeout は両方に届く。ES 単独の失敗はサーバーの別処理を止めない。")
}

// ============================================================
// 表示用ヘルパ
// ============================================================

func section(title string) {
	fmt.Printf("\n==================== %s ====================\n", title)
}

func report(label string, err error) {
	switch {
	case err == nil:
		fmt.Printf("    %s: 成功\n", label)
	case errors.Is(err, esfake.ErrBudgetExceeded):
		fmt.Printf("    %s: ES 個別予算超過 -> %v\n", label, err)
	case errors.Is(err, context.DeadlineExceeded):
		fmt.Printf("    %s: DeadlineExceeded(サーバー由来) -> %v\n", label, err)
	case errors.Is(err, context.Canceled):
		fmt.Printf("    %s: Canceled -> %v\n", label, err)
	default:
		fmt.Printf("    %s: その他 -> %v\n", label, err)
	}
}

func reportCtxState(name string, ctx context.Context) {
	if err := ctx.Err(); err != nil {
		fmt.Printf("    %s の状態: 終了済み (Err=%v)\n", name, err)
	} else {
		fmt.Printf("    %s の状態: まだ有効\n", name)
	}
}
