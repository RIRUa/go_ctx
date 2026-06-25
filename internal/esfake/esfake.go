// Package esfake は Elasticsearch クライアントを使わずに「context を尊重する
// 外部呼び出し」を模倣するためのパッケージです。
//
// 実際の Elasticsearch Go クライアントは、渡された context が Done になると
// 進行中のリクエストを中断し、context のエラー（context.DeadlineExceeded や
// context.Canceled）を返します。ここではネットワーク I/O の代わりに time.After で
// 「処理にかかる時間」を表現し、その間 ctx.Done() を監視することで同じ挙動を再現します。
//
// 重要な設計方針:
//
//	ES 個別のタイムアウト context は「呼び出し側」ではなく「ES クライアント(=この
//	パッケージ)の中」で作ります。こうすると cancel をクライアント内で defer でき、
//	呼び出し側が defer cancel を書き忘れて context をリークする事故を防げます。
//	呼び出し側はサーバーのリクエスト context（親）を渡すだけで済みます。
package esfake

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// ErrBudgetExceeded は ES 個別のタイムアウト予算を超過したことを表す cause です。
// サーバー(親)由来の打ち切りと、ES 個別予算の超過を errors.Is で区別するために使います。
var ErrBudgetExceeded = errors.New("es individual budget exceeded")

// Client は ES クライアントを模したものです。
//
//   - Timeout: ES 1 呼び出しあたりの個別タイムアウト予算。
//   - Latency: ES が応答を返すまでにかかる時間（実験用のシミュレーション値）。
type Client struct {
	Timeout time.Duration
	Latency time.Duration
}

// New は Client を生成します。
func New(timeout, latency time.Duration) *Client {
	return &Client{Timeout: timeout, Latency: latency}
}

// Search はパターン1（親子・派生）の ES 呼び出しです。
//
// 呼び出し側はサーバーのリクエスト context（parent）を渡すだけ。ES 個別の timeout
// context はこの関数の中で派生させ、defer cancel で必ず解放します（← cancel 忘れ防止）。
//
//   - 親(サーバー)が先に切れれば、その deadline 超過が cause になる。
//   - ES 個別予算(Timeout)が先に切れれば、cause は ErrBudgetExceeded になる。
//
// 子(ES)の打ち切りは parent には伝播しないので、呼び出し側は parent.Err() を見て
// 「サーバーはまだ生きているか」を判断し、フォールバックに進めます。
func (c *Client) Search(parent context.Context, query string) (string, error) {
	// ★ esCtx はサーチ関数の中で作り、ここで defer cancel する。
	ctx, cancel := context.WithTimeoutCause(parent, c.Timeout, ErrBudgetExceeded)
	defer cancel()
	return c.run(ctx, query)
}

// SearchWithServerPropagation はパターン2（並列・独立 + 親→子の一方向伝播）の
// ES 呼び出しです。
//
// esCtx を serverCtx の子にはせず、Background から独立に作ります（＝並列）。その上で
// context.AfterFunc によってサーバー(親)の終了『だけ』を esCtx へ一方向に伝播させます。
// ES 個別予算の打ち切りは独立 root に閉じるので、サーバーには一切影響しません。
//
// ここでも context の生成・後始末（cancel / stop）はすべて関数内で defer します。
func (c *Client) SearchWithServerPropagation(serverCtx context.Context, query string) (string, error) {
	// 独立した root（cause 付き）。
	base, cancelCause := context.WithCancelCause(context.Background())
	defer cancelCause(context.Canceled)

	// ES 個別の timeout。超過時の cause は ErrBudgetExceeded。
	ctx, cancelTimer := context.WithTimeoutCause(base, c.Timeout, ErrBudgetExceeded)
	defer cancelTimer()

	// 親(サーバー)が切れたら esCtx だけをキャンセル（親→子の一方向伝播）。
	stop := context.AfterFunc(serverCtx, func() {
		cancelCause(fmt.Errorf("server context done: %w", context.Cause(serverCtx)))
	})
	defer stop()

	return c.run(ctx, query)
}

// run は実際の「ネットワーク I/O」を模倣する内部処理です。ctx が切れたら、その
// 原因を context.Cause で取り出して返します（呼び出し側が errors.Is で
// ErrBudgetExceeded / context.DeadlineExceeded を判別できるように）。
func (c *Client) run(ctx context.Context, query string) (string, error) {
	if dl, ok := ctx.Deadline(); ok {
		fmt.Printf("    [esfake] Search: query=%q latency=%v deadline まで残り %v\n",
			query, c.Latency, time.Until(dl).Round(time.Millisecond))
	}

	select {
	case <-time.After(c.Latency):
		return fmt.Sprintf("ok(query=%s)", query), nil
	case <-ctx.Done():
		// 実 ES クライアントは ctx.Err() を返しますが、ここでは原因を保ったまま
		// 返したいので context.Cause を使います（WithCancelCause 経由だと
		// ctx.Err() は一律 Canceled になり、本当の原因が失われるため）。
		return "", context.Cause(ctx)
	}
}

// Search は、与えられた ctx をそのまま尊重する低レベルの呼び出しです。
//
// 通常は上の Client を使ってください。これは「呼び出し側が任意に組み立てた context
// （独立・merge など）を渡したときの挙動」を観察する実験用に残してあります。
func Search(ctx context.Context, query string, work time.Duration) (string, error) {
	if dl, ok := ctx.Deadline(); ok {
		fmt.Printf("    [esfake] Search 開始: query=%q 想定処理時間=%v 受け取った deadline まで残り %v\n",
			query, work, time.Until(dl).Round(time.Millisecond))
	} else {
		fmt.Printf("    [esfake] Search 開始: query=%q 想定処理時間=%v deadline 無し\n", query, work)
	}

	select {
	case <-time.After(work):
		return fmt.Sprintf("ok(query=%s)", query), nil
	case <-ctx.Done():
		return "", ctx.Err()
	}
}
