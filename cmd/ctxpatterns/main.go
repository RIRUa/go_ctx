// Command ctxpatterns は、検討中の 2 つの context 設計パターンを、サーバーや
// Elasticsearch を使わずに動かして比較するための実験プログラムです。
//
// パターン1 (親子・派生):
//
//	サーバー context（親）から ES 用 context（子）を派生させる。子(ES)の timeout が
//	切れても、親(サーバー)は生かしてフォールバック処理や部分応答を続けたい、という形。
//
// パターン2 (並列・独立 + 一方向伝播):
//
//	サーバー context と ES context を「並列に（独立して）」作り、サーバー(親)の
//	timeout『だけ』を子(ES)へ一方向に伝播させる形。ES 側の打ち切りは親に影響しない。
//
// 結論の先取り:
//   - 実行時のキャンセル挙動だけ見ると、パターン2はパターン1（派生）と同じ結果に
//     なります。パターン2は派生で自動的に得られる「親→子の伝播」を手作業で組み直して
//     いるだけなので、特別な理由が無ければパターン1（派生）を勧めます。
//   - パターン2が本当に要るのは「子の deadline を親より長くしたい / 親より独立に
//     管理したいが、親が死んだら子も止めたい」といった、派生では表現しづらいケース。
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
//	serverCtx (500ms) ─派生→ esCtx (100ms)
//	ES 処理は 300ms かかる想定 → esCtx が先に切れる。serverCtx はまだ生きている。
func pattern1ChildTimesOutParentSurvives() {
	section("パターン1: 子(ES)が timeout → 親(サーバー)は生存しフォールバック")

	serverCtx, cancelServer := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancelServer()

	// ES 呼び出し直前に、サーバー context を親にして個別 timeout を派生。
	esCtx, cancelES := context.WithTimeout(serverCtx, 100*time.Millisecond)
	defer cancelES()

	_, err := esfake.Search(esCtx, "primary", 300*time.Millisecond)
	report("ES(primary)", err)

	// ここがパターン1の肝: ES がコケても親 context の Err を見て分岐する。
	if errors.Is(err, context.DeadlineExceeded) && serverCtx.Err() == nil {
		fmt.Println("    -> ES はタイムアウトしたが serverCtx は生存。フォールバックに進む。")

		// 親 context にはまだ予算が残っているので、後続処理（軽い再検索や
		// キャッシュ参照、部分応答の組み立て）を続けられる。
		fbCtx, cancelFB := context.WithTimeout(serverCtx, 100*time.Millisecond)
		defer cancelFB()
		res, fbErr := esfake.Search(fbCtx, "fallback", 30*time.Millisecond)
		report("ES(fallback)", fbErr)
		fmt.Printf("    クライアントへ返す結果: %q (degraded)\n", res)
	}
	reportCtxState("serverCtx", serverCtx)
	fmt.Println("=> 狙い通り。子の打ち切りは親に伝播しないので、親で安全にハンドリングできる。")
}

// pattern1ParentTimesOut は、親(サーバー)の予算自体が尽きた場合は、派生した子も
// 巻き込まれて止まる（＝リクエスト全体を諦める）ことを示します。フォールバックも
// 同じ serverCtx 由来なので動けません。
func pattern1ParentTimesOut() {
	section("パターン1: 親(サーバー)の予算が尽きた場合")

	serverCtx, cancelServer := context.WithTimeout(context.Background(), 80*time.Millisecond)
	defer cancelServer()
	esCtx, cancelES := context.WithTimeout(serverCtx, 500*time.Millisecond)
	defer cancelES()

	_, err := esfake.Search(esCtx, "primary", 300*time.Millisecond)
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

// errESBudget は ES 個別予算の超過を表す cause です。原因の読み分けに使います。
var errESBudget = errors.New("es individual budget exceeded")

// newESContextWithServerPropagation は、パターン2の中心となるヘルパです。
//
//   - esCtx は serverCtx の「子」ではなく、Background から独立に作る（＝並列）。
//   - esCtx には ES 個別の timeout を持たせる。
//   - context.AfterFunc で serverCtx.Done() を監視し、サーバーが切れたら esCtx だけを
//     キャンセルする（親→子の一方向伝播）。逆向き（子→親）は一切起きない。
//
// stop() は AfterFunc の登録を解除します（ES が先に終わったときの後始末用）。
func newESContextWithServerPropagation(serverCtx context.Context, esBudget time.Duration) (
	ctx context.Context, cancel context.CancelFunc, stop func() bool,
) {
	// 独立した root に cause 付き cancel を持たせる。
	base, cancelCause := context.WithCancelCause(context.Background())

	// ES 個別の timeout。超過時の cause は errESBudget。
	esCtx, cancelTimer := context.WithTimeoutCause(base, esBudget, errESBudget)

	// 親(サーバー)が切れたら esCtx だけをキャンセル。原因は serverCtx 側の cause を引き継ぐ。
	stop = context.AfterFunc(serverCtx, func() {
		cancelCause(fmt.Errorf("server context done: %w", context.Cause(serverCtx)))
	})

	// 呼び出し側に渡す後始末: タイマー解除 + cause cancel。
	cancel = func() {
		cancelTimer()
		cancelCause(context.Canceled)
	}
	return esCtx, cancel, stop
}

// pattern2PropagateToChildOnly_ServerTimesOut は、独立に作った esCtx に対して
// サーバー context の timeout が一方向で伝播し、ES 呼び出しが中断されることを示します。
//
//	serverCtx (80ms, 独立)
//	esCtx     (500ms, 独立) ← AfterFunc で serverCtx.Done を受けて中断
//	ES 処理は 300ms 想定 → サーバー(80ms)が先に切れ、その伝播で esCtx も切れる。
func pattern2PropagateToChildOnly_ServerTimesOut() {
	section("パターン2: 独立 esCtx へサーバー timeout が一方向伝播（サーバーが先に切れる）")

	serverCtx, cancelServer := context.WithTimeout(context.Background(), 80*time.Millisecond)
	defer cancelServer()

	esCtx, cancelES, stop := newESContextWithServerPropagation(serverCtx, 500*time.Millisecond)
	defer stop()
	defer cancelES()

	_, err := esfake.Search(esCtx, "es-parallel", 300*time.Millisecond)
	report("ES", err)
	// esCtx.Err() は WithCancelCause 由来なので Canceled。本当の原因は Cause で読む。
	fmt.Printf("    esCtx.Err()=%v / context.Cause(esCtx)=%v\n", esCtx.Err(), context.Cause(esCtx))
	fmt.Println("=> サーバーの timeout が esCtx に伝播して ES を中断。Cause からサーバー由来と分かる。")
}

// pattern2PropagateToChildOnly_ESTimesOut は、逆に ES 個別予算が先に切れた場合、
// サーバー context は一切影響を受けない（子→親は伝播しない）ことを示します。
func pattern2PropagateToChildOnly_ESTimesOut() {
	section("パターン2: ES 個別予算が先に切れる（サーバーは無傷）")

	serverCtx, cancelServer := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancelServer()

	esCtx, cancelES, stop := newESContextWithServerPropagation(serverCtx, 80*time.Millisecond)
	defer stop()
	defer cancelES()

	_, err := esfake.Search(esCtx, "es-parallel", 300*time.Millisecond)
	report("ES", err)
	fmt.Printf("    context.Cause(esCtx)=%v (ES 個別予算の超過)\n", context.Cause(esCtx))
	reportCtxState("serverCtx", serverCtx)
	fmt.Println("=> ES の打ち切りは独立 root に閉じるのでサーバーは無傷。パターン1と同じ結果。")
}

// pattern2Parallel は「二つ並列でやらせる」イメージそのものを再現します。
//
// メイン goroutine はサーバー処理を進めつつ、ES 呼び出しを別 goroutine で並走させます。
// サーバー context の timeout は両方に効きます（ES へは AfterFunc 経由）。ES が単独で
// コケてもサーバー側の別処理は止まりません。
func pattern2Parallel() {
	section("パターン2: 二つ並列（ES を別 goroutine で並走、サーバー timeout が両方に効く）")

	serverCtx, cancelServer := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancelServer()

	type esResult struct {
		body string
		err  error
	}
	esDone := make(chan esResult, 1)

	// 並走その1: ES 呼び出し（独立 ctx + サーバー伝播）。
	go func() {
		esCtx, cancelES, stop := newESContextWithServerPropagation(serverCtx, 150*time.Millisecond)
		defer stop()
		defer cancelES()
		body, err := esfake.Search(esCtx, "parallel-es", 100*time.Millisecond)
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
	case errors.Is(err, errESBudget):
		fmt.Printf("    %s: ES 個別予算超過 -> %v\n", label, err)
	case errors.Is(err, context.DeadlineExceeded):
		fmt.Printf("    %s: DeadlineExceeded -> %v\n", label, err)
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
