// Package esfake は Elasticsearch クライアントを使わずに「context を尊重する
// 外部呼び出し」を模倣するためのパッケージです。
//
// 実際の Elasticsearch Go クライアントは、渡された context が Done になると
// 進行中のリクエストを中断し、context のエラー（context.DeadlineExceeded や
// context.Canceled）を返します。ここではネットワーク I/O の代わりに time.After で
// 「処理にかかる時間」を表現し、その間 ctx.Done() を監視することで同じ挙動を再現します。
package esfake

import (
	"context"
	"fmt"
	"time"
)

// Search は ES への検索リクエストを模倣します。
//
//   - work: その呼び出しが「本来かかる処理時間」。
//
// 戻り値:
//   - work の時間内に処理が終われば結果文字列を返す。
//   - その前に ctx が Done になれば ctx.Err()（DeadlineExceeded か Canceled）を返す。
//
// これは実際の ES クライアントが「context が切れたら即座に中断してエラーを返す」
// 挙動と同じ形をしています。
func Search(ctx context.Context, query string, work time.Duration) (string, error) {
	// 呼び出し時点で渡された context にどんな deadline が乗っているかを覗いておく。
	// 「サーバーの context」と「ES 直前の context」のどちらが効いているのかを
	// 観察するのに役立ちます。
	if dl, ok := ctx.Deadline(); ok {
		fmt.Printf("    [esfake] Search 開始: query=%q 想定処理時間=%v 受け取った deadline まで残り %v\n",
			query, work, time.Until(dl).Round(time.Millisecond))
	} else {
		fmt.Printf("    [esfake] Search 開始: query=%q 想定処理時間=%v deadline 無し\n", query, work)
	}

	select {
	case <-time.After(work):
		// 処理が context より先に完了したケース。
		return fmt.Sprintf("ok(query=%s)", query), nil
	case <-ctx.Done():
		// context が先に切れたケース。実 ES クライアントと同様に ctx.Err() を返す。
		return "", ctx.Err()
	}
}
