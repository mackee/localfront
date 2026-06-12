# Fidelity notes — 実 CloudFront での要検証項目

ドキュメントから断定できず、実 CloudFront での一括検証日に確認してテスト期待値へ反映する項目。
検証したら結果と日付を追記し、対応するコード/テストを更新する。

| # | 項目 | localfront の現在の挙動 | 確認すること | 状態 |
|---|---|---|---|---|
| 1 | 署名付き URL / Cookie 検証と viewer-request function の実行順序 | 未実装（M5/M6） | 署名検証が function の前か後か（function で URI を書き換えた場合に署名対象がどちらの URI か） | 未検証 |
| 2 | 非許可メソッドへの応答 | 403 + 「could not be satisfied」ページ | 実 CloudFront が 403 か 405 か、本文・`Allow` ヘッダの有無 | 未検証 |
| 3 | S3 オリジンの `NoSuchKey` | レスポンスをそのまま透過（起動時クレデンシャルは全アクセス権を持つため欠損キーは 404 NoSuchKey で返る、AccessDenied は 403） | 実 CloudFront は OAC の ListBucket 権限有無で 403/404 が変わる。OAC 非強制の localfront では「ストアが返す素のステータスを透過」=実質 404。実 CloudFront の素の S3 エラーページ本文との細部差分は未対照 | RustFS で 404/206/304 の透過を確認済み（M3 e2e）。実 CloudFront 本文は未対照 |
| 4 | 未知 Host への応答 | 403 + CF 風 HTML | 実 CloudFront の 403 本文・ヘッダ（`Server: CloudFront` 等）との細部差分 | 未検証 |
| 5 | match-viewer プロトコルポリシー | viewer 側が常に HTTP のため http 固定 | TLS 終端（roadmap）導入時に https 移行 | 設計どおり |
| 6 | オリジンへの `Host` ヘッダ | 常にオリジンドメインに置換（M2） | cache/origin-request ポリシーで viewer Host を転送する場合の挙動（M4 で policy 駆動に変更予定） | M4 で対応 |
| 7 | `X-Cache` の値 | `Miss from localfront` / `Error from localfront` | README 仕様（意図的に cloudfront と変えている）。テストはこの値に依存して良い | 仕様確定 |
| 8 | viewer-request function の URI 書き換え後の behavior 再評価 | 再評価しない予定（M5） | CloudFront 仕様: function は behavior 解決後・キャッシュキー前。AWS 公式ドキュメントの記載で確認済みだがテストで担保する | ドキュメント根拠あり |
| 9 | 圧縮（gzip/brotli）の対象 Content-Type とサイズ閾値 | 未実装(M4) | AWS ドキュメントの表（1,000 bytes 以上 10MB 以下等）をそのまま採用して良いか | 未検証 |
| 10 | `ConnectionAttempts` / `ConnectionTimeout` のリトライ挙動 | 未配線（共有 dialer 10s 固定） | 接続失敗時のリトライ回数・対象（接続のみ、レスポンス待ちは対象外のはず） | M4 で対応 |
