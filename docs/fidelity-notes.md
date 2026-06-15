# Fidelity notes — 実 CloudFront での要検証項目

ドキュメントから断定できず、実 CloudFront での一括検証日に確認してテスト期待値へ反映する項目。
検証したら結果と日付を追記し、対応するコード/テストを更新する。

| # | 項目 | localfront の現在の挙動 | 確認すること | 状態 |
|---|---|---|---|---|
| 1 | 署名付き URL / Cookie 検証と viewer-request function の実行順序 | 署名検証を viewer-request function の**前**に実行（M6） | 署名検証が function の前か後か（function で URI を書き換えた場合に署名対象がどちらの URI か） | function 前に実装。実 CloudFront 順序は未検証 |
| 15 | 署名付き URL の Resource とリクエストの一致判定 | canned: scheme を https→http の順で再構成して署名一致を確認（host は Host ヘッダ=ポート込み、path はリクエストパス、query は署名パラメータ Expires/Signature/Key-Pair-Id/Policy/Hash-Algorithm を除いたアプリのクエリ）。custom: Policy 内 Resource の path 部分のみワイルドカード照合し scheme/host は無視 | 実 CloudFront の resource 照合の厳密さ（scheme/host/port を含むか、エラーページ等で query を含むか） | 未検証（path+query 照合 + canned は scheme 二択。port は「ローカルの配信ホスト:ポートに対して署名する」前提で Host ヘッダ込み） |
| 16 | Key-Pair-Id | localfront が logical ID から生成する PublicKey ID（`K...`）を使う必要がある（実 CloudFront の鍵 ID とは異なる） | — | 仕様（決定的生成のため） |
| 2 | 非許可メソッドへの応答 | 403 + 「could not be satisfied」ページ | 実 CloudFront が 403 か 405 か、本文・`Allow` ヘッダの有無 | 未検証 |
| 3 | S3 オリジンの `NoSuchKey` | レスポンスをそのまま透過（起動時クレデンシャルは全アクセス権を持つため欠損キーは 404 NoSuchKey で返る、AccessDenied は 403）。404/403 は custom error responses（M4）の対象となり SPA フォールバックが効く | 実 CloudFront は OAC の ListBucket 権限有無で 403/404 が変わる。OAC 非強制の localfront では「ストアが返す素のステータスを透過」=実質 404。実 CloudFront の素の S3 エラーページ本文との細部差分は未対照 | RustFS で 404/206/304 の透過 + SPA フォールバックを確認済み（M3/M8 e2e）。実 CloudFront 本文は未対照 |
| 4 | 未知 Host への応答 | 403 + CF 風 HTML | 実 CloudFront の 403 本文・ヘッダ（`Server: CloudFront` 等）との細部差分 | 未検証 |
| 5 | match-viewer プロトコルポリシー | viewer 側が常に HTTP のため http 固定 | TLS 終端（roadmap）導入時に https 移行 | 設計どおり |
| 6 | オリジンへの `Host` ヘッダ | cache/origin-request ポリシーが Host を選択していれば viewer Host を転送、そうでなければオリジンドメインに置換（M4 で policy 駆動化） | 実 CloudFront での Host 転送条件（カスタムオリジンのみか等）の細部 | M4 で policy 駆動に変更済み。細部は未検証 |
| 7 | `X-Cache` の値 | `Miss from localfront` / `Error from localfront` | README 仕様（意図的に cloudfront と変えている）。テストはこの値に依存して良い | 仕様確定 |
| 8 | viewer-request function の URI 書き換え後の behavior 再評価 | 再評価しない（M5 で実装、テストで担保） | CloudFront 仕様: function は behavior 解決後・キャッシュキー前 | テストで担保済み（M5） |
| 13 | viewer-request function が追加/変更したヘッダのオリジン到達 | function は behavior 解決後・origin request policy 適用前に動くモデルとし、function が返したリクエストヘッダも cache/origin-request ポリシーでフィルタされる（policy が none なら届かない）。URI・クエリ書き換えは policy 非依存で反映 | 実 CloudFront が function 追加ヘッダを policy 非依存でオリジンへ転送するか | 未検証（policy フィルタ採用） |
| 14 | viewer-response function のヘッダ適用 | function が返したヘッダ集合でレスポンスヘッダを置換（受信した全ヘッダを渡し、返り値で上書き）。statusCode も反映 | 実 CloudFront のマージ/置換セマンティクスの細部 | 未検証 |
| 9 | 圧縮（gzip/brotli）の対象 Content-Type とサイズ閾値 | AWS ドキュメントの表を採用（compressible Content-Type のみ、`Content-Length` が 1,000〜10,000,000 bytes、`Content-Encoding` 未設定、`Cache-Control: no-transform` でない、ステータス 200、GET）。両方受理時は **gzip を優先**（決定性のため。実 CloudFront の優先順位は未確定） | 実 CloudFront の gzip/brotli 優先順位、Content-Length 不明時の挙動 | M4 で実装。優先順位は未検証 |
| 10 | `ConnectionAttempts` / `ConnectionTimeout` のリトライ挙動 | 未配線（共有 dialer 10s 固定）。M4 では未対応のまま | 接続失敗時のリトライ回数・対象（接続のみ、レスポンス待ちは対象外のはず） | 未対応（PoC では優先度低、roadmap） |
| 11 | CORS プリフライト（OPTIONS）の処理 | localfront は OPTIONS をオリジンへ転送し、response headers policy の CORS ヘッダを応答に付与する（短絡応答しない）。behavior が OPTIONS を許可していなければ 403 | 実 CloudFront が OPTIONS を短絡応答するか、必ずオリジンに転送するか。response headers policy のみで preflight が成立するか | 未検証（ドキュメントからは「オリジンに転送 + ヘッダ付与」と解釈） |
| 12 | `CloudFront-*` ヘッダのケーシング | Go の `http.Header` 正規化で `Cloudfront-Viewer-Country` 等になる（実 CloudFront は `CloudFront-Viewer-Country`）。HTTP ヘッダは大文字小文字非依存のため機能影響なし | — | 仕様（PoC では許容） |
