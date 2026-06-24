// Command ctxdemo は context.Deadline の挙動を、サーバーや Elasticsearch を使わずに
// 確認するための実験プログラムです。
//
// 確認したいこと:
//  1. サーバー側の context（リクエスト全体の deadline）と、ES 呼び出し直前に作る
//     context（ES 個別の timeout）は「競合」するのか?
//     → 親 context から WithTimeout で派生させた場合、両者の deadline のうち
//     「より早い方」が必ず勝ちます。競合してどちらか不定になることはありません。
//  2. もし派生関係にせず「別々の独立した context」を作った場合、相手の Done を
//     どう検知・ハンドリングすればよいか?
//     → 子は親の Done を自動では受け取れないので、明示的に両方の Done を監視する
//     必要があります（merge する）。
//  3. context が切れたとき ctx.Err() から DeadlineExceeded / Canceled を読み分けられるか。
//
// 実行: go run ./cmd/ctxdemo
package main

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/rirua/go_ctx/internal/esfake"
)

func main() {
	experiment1DerivedShorterChild()
	experiment2DerivedShorterParent()
	experiment3DerivedExactValues()
	experiment4IndependentContexts()
	experiment5MergeIndependentContexts()
}

// experiment1DerivedShorterChild は典型的なケースです。
//
// サーバー context（長め: 500ms）から、ES 呼び出し直前に短い timeout（100ms）を
// 派生させます。ES の処理は 300ms かかる想定。
//
// 期待: ES 個別 timeout(100ms) が先に切れる。サーバー context はまだ生きている。
func experiment1DerivedShorterChild() {
	section("実験1: 派生 context / 子(ES)の timeout が短い")

	// サーバーがリクエストを受けたときに張る context。
	serverCtx, cancelServer := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancelServer()

	// ES を呼ぶ直前に、サーバー context を「親」にして個別の timeout を張る。
	esCtx, cancelES := context.WithTimeout(serverCtx, 100*time.Millisecond)
	defer cancelES()

	_, err := esfake.Search(esCtx, "shorter-child", 300*time.Millisecond)
	report("ES 呼び出し", err)

	// 子が切れてもサーバー context 自体はまだ有効。
	reportCtxState("serverCtx", serverCtx)
	reportCtxState("esCtx", esCtx)
	fmt.Println("=> 子(ES)の deadline が親より早いので子が勝つ。サーバー context は無傷。")
}

// experiment2DerivedShorterParent は逆のケースです。
//
// サーバー context が短く(100ms)、ES 直前に張った個別 timeout が長い(500ms)。
// ES の処理は 300ms 想定。
//
// 期待: サーバー context(100ms) が先に切れ、ES 個別 timeout は一度も発火しない。
// つまり子の deadline 設定に関わらず「親の方が早ければ親が勝つ」。
func experiment2DerivedShorterParent() {
	section("実験2: 派生 context / 親(サーバー)の deadline が短い")

	serverCtx, cancelServer := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancelServer()

	// ES 個別には余裕を持たせたつもり(500ms)でも…
	esCtx, cancelES := context.WithTimeout(serverCtx, 500*time.Millisecond)
	defer cancelES()

	// 派生 context の Deadline() は親子の早い方になる。実際に覗いてみる。
	if dl, ok := esCtx.Deadline(); ok {
		fmt.Printf("    esCtx.Deadline() までの残り: %v (500ms ではなく親の 100ms 側に丸められる)\n",
			time.Until(dl).Round(time.Millisecond))
	}

	_, err := esfake.Search(esCtx, "shorter-parent", 300*time.Millisecond)
	report("ES 呼び出し", err)

	reportCtxState("serverCtx", serverCtx)
	reportCtxState("esCtx", esCtx)
	fmt.Println("=> 親(サーバー)が早く切れると、子の長い timeout は意味を持たず親が勝つ。")
}

// experiment3DerivedExactValues は、派生 context の Deadline() が必ず
// 「親と子の早い方」になることを、値レベルで突き合わせて確認します。
func experiment3DerivedExactValues() {
	section("実験3: 派生 context の Deadline() は親子の早い方になる(値で確認)")

	parent, cancelP := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancelP()
	parentDL, _ := parent.Deadline()

	// 親より遅い deadline を要求しても…
	childLate, cancelCL := context.WithDeadline(parent, parentDL.Add(1*time.Second))
	defer cancelCL()
	childLateDL, _ := childLate.Deadline()
	fmt.Printf("    親より遅い deadline を要求 -> 実際の childLate.Deadline() == 親の deadline か?: %v\n",
		childLateDL.Equal(parentDL))

	// 親より早い deadline を要求した場合は、その早い方が採用される。
	childEarly, cancelCE := context.WithDeadline(parent, parentDL.Add(-100*time.Millisecond))
	defer cancelCE()
	childEarlyDL, _ := childEarly.Deadline()
	fmt.Printf("    親より早い deadline を要求 -> childEarly は親より早いか?: %v (差 %v)\n",
		childEarlyDL.Before(parentDL), parentDL.Sub(childEarlyDL).Round(time.Millisecond))
	fmt.Println("=> WithDeadline/WithTimeout は親の deadline を超えて延長できない。短くする方向のみ有効。")
}

// experiment4IndependentContexts は「別々の(派生関係にない)context」を作った場合に、
// 一方が他方の Done を自動では検知しないことを示します。
//
// serverCtx と esCtx を、どちらも context.Background() から独立に作ります。
// serverCtx が先に切れても、esCtx を渡した呼び出しはそれに気づきません。
func experiment4IndependentContexts() {
	section("実験4: 独立 context / 相手の Done は自動では伝わらない")

	// サーバー context（独立、100ms で切れる）。
	serverCtx, cancelServer := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancelServer()

	// ES context を Background から「独立に」作る（serverCtx を親にしていない）。
	esCtx, cancelES := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancelES()

	// serverCtx は 100ms で切れるが、esCtx はそれを知らないので 300ms の処理を続行できる。
	_, err := esfake.Search(esCtx, "independent", 300*time.Millisecond)
	report("ES 呼び出し(esCtx だけを渡した)", err)

	reportCtxState("serverCtx", serverCtx) // すでに切れているはず
	reportCtxState("esCtx", esCtx)         // まだ生きている
	fmt.Println("=> 独立 context では親子のような自動伝播が無い。サーバーが諦めても ES 呼び出しは止まらない。")
}

// experiment5MergeIndependentContexts は、独立した 2 つの context を扱う場合に
// 相手の Done を「明示的に」検知してハンドリングする方法を 2 通り示します。
//
//   - 方法A: select で両方の Done を同時に監視する。
//   - 方法B: 2 つの context を 1 つに merge した context を作って下流へ渡す。
//
// 実運用では、可能なら派生(親子)にするのが最も安全で簡単です。どうしても独立に
// 作らざるを得ない場合のみ、こうした merge が必要になります。
func experiment5MergeIndependentContexts() {
	section("実験5: 独立 context を merge して相手の Done をハンドリングする")

	serverCtx, cancelServer := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancelServer()
	esCtx, cancelES := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancelES()

	// --- 方法A: select で両方を監視 ---
	fmt.Println("  [方法A] select で serverCtx と esCtx を同時に監視")
	done := make(chan struct{})
	go func() {
		// ES 処理(300ms)を別 goroutine で実行し、その完了を待つ想定。
		time.Sleep(300 * time.Millisecond)
		close(done)
	}()
	select {
	case <-done:
		fmt.Println("    ES 処理が完了")
	case <-serverCtx.Done():
		// サーバーが諦めたことを検知できる。何が原因かは Err() で分かる。
		fmt.Printf("    serverCtx.Done() を検知 -> 中断。理由: %v\n", serverCtx.Err())
	case <-esCtx.Done():
		fmt.Printf("    esCtx.Done() を検知 -> 中断。理由: %v\n", esCtx.Err())
	}

	// --- 方法B: 2 つの context を merge して 1 つの context にする ---
	fmt.Println("  [方法B] merge した context を下流(esfake.Search)に渡す")
	serverCtx2, cancelServer2 := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancelServer2()
	esCtx2, cancelES2 := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancelES2()

	merged, cancelMerged := mergeContexts(serverCtx2, esCtx2)
	defer cancelMerged()

	// merged はどちらか一方でも Done になれば Done になる。下流はこれ 1 つを見れば良い。
	_, err := esfake.Search(merged, "merged", 300*time.Millisecond)
	report("ES 呼び出し(merged)", err)
	// 注意: merged は内部で WithCancelCause により中断されるため、merged.Err() は
	// 一律 context.Canceled になる。本当の中断理由(どちらが先に切れたか)は
	// context.Cause() で取り出す。
	fmt.Printf("    merged.Err()=%v / context.Cause(merged)=%v\n", merged.Err(), context.Cause(merged))
	fmt.Printf("    serverCtx2.Err()=%v / esCtx2.Err()=%v\n", serverCtx2.Err(), esCtx2.Err())
	fmt.Println("=> merge すれば独立 context でも『どちらか早い方の Done』を 1 本で扱える。")
	fmt.Println("   理由の読み分けは context.Cause() を使う(serverCtx2 の deadline 超過が原因と分かる)。")
}

// mergeContexts は 2 つの context を合成し、どちらか一方が Done になったら
// Done になる新しい context を返します。Go 1.21+ なら標準ライブラリの
// context.WithoutCancel/AfterFunc などで似た事は書けますが、ここでは挙動を
// 明示するため goroutine で両方の Done を監視する素朴な実装にしています。
//
// 返す context の Err()/Deadline() は「先に切れた方」の値を反映します。
func mergeContexts(a, b context.Context) (context.Context, context.CancelFunc) {
	// cause を伝えるため WithCancelCause を使う。
	merged, cancel := context.WithCancelCause(context.Background())

	// 親 a, b の deadline のうち早い方を merged にも反映しておくと、
	// merged.Deadline() が呼ばれたとき正しい値を返せる。
	if dl, ok := earliestDeadline(a, b); ok {
		var c context.CancelFunc
		merged, c = context.WithDeadline(merged, dl)
		// cause を保持したいので、先に cause 付きの cancel を呼んでから
		// WithDeadline 側の後始末をする。順番を逆にすると merged の Cause が
		// 単なる context.Canceled に潰れてしまう。
		causeCancel := cancel
		cancel = func(err error) {
			causeCancel(err) // cause を merged(子)へ伝播させる
			c()              // WithDeadline タイマーの後始末
		}
	}

	go func() {
		select {
		case <-a.Done():
			cancel(a.Err())
		case <-b.Done():
			cancel(b.Err())
		case <-merged.Done():
			// merged 自身が(呼び出し側の cancel で)終わったら監視を終える。
		}
	}()

	// 呼び出し側には引数なしの CancelFunc を見せる。
	return merged, func() { cancel(context.Canceled) }
}

// earliestDeadline は 2 つの context の deadline のうち早い方を返します。
// どちらも deadline を持たなければ ok=false。
func earliestDeadline(a, b context.Context) (time.Time, bool) {
	da, oka := a.Deadline()
	db, okb := b.Deadline()
	switch {
	case oka && okb:
		if da.Before(db) {
			return da, true
		}
		return db, true
	case oka:
		return da, true
	case okb:
		return db, true
	default:
		return time.Time{}, false
	}
}

// --- 以下、表示用ヘルパ ---

func section(title string) {
	fmt.Printf("\n==================== %s ====================\n", title)
}

func report(label string, err error) {
	switch {
	case err == nil:
		fmt.Printf("    %s: 成功(context は切れなかった)\n", label)
	case errors.Is(err, context.DeadlineExceeded):
		fmt.Printf("    %s: DeadlineExceeded（deadline 超過）-> %v\n", label, err)
	case errors.Is(err, context.Canceled):
		fmt.Printf("    %s: Canceled（明示キャンセル）-> %v\n", label, err)
	default:
		fmt.Printf("    %s: その他のエラー -> %v\n", label, err)
	}
}

func reportCtxState(name string, ctx context.Context) {
	if err := ctx.Err(); err != nil {
		fmt.Printf("    %s の状態: 終了済み (Err=%v)\n", name, err)
	} else {
		fmt.Printf("    %s の状態: まだ有効\n", name)
	}
}
