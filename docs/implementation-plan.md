# localfront 実装計画

README.md で定義した PoC スコープを実装するための計画。各マイルストーンに検証用テストを含め、最後に現実世界のユースケースに沿った統合テストシナリオを定義する。

## 全体方針

- **早期に縦のスライスを貫通させる**: 「テンプレート読み込み → 1 distribution → カスタムオリジンに proxy → curl で確認」を最初の 2 マイルストーンで成立させ、以降は機能を肉付けする。
- **リスクの高い技術選定はスパイクで先に潰す**: CFN パーサ(goformation 採用可否)と JS エンジン(goja の cloudfront-js 2.0 互換性)の 2 つが最大の不確定要素。
- **CloudFront の実挙動が曖昧な点は「fidelity notes」として記録する**: ドキュメントから断定できない挙動(署名検証と Function 実行の順序、405/403 の使い分け等)は、実 CloudFront で一度検証してテストの期待値に落とす。
- **テストは各マイルストーンの Definition of Done に含める**: 実装とテストを同一 PR で完結させる。

## パッケージ構成(予定)

```
cmd/localfront/      CLI (serve サブコマンド、フラグ、slog セットアップ)
internal/cfntmpl/    CFN テンプレートのパース、intrinsic 関数の解決、検証
internal/config/     解決済みモデル (Distribution/Behavior/Origin/Policy...)、
                     マネージドポリシー定義 (embed)、distribution ID 生成
internal/dataplane/  HTTP サーバ、Host ルーティング、リクエストパイプライン
internal/behavior/   path pattern マッチング、cache/origin-request/response-headers
                     ポリシーのセマンティクス
internal/origin/     カスタムオリジン proxy、S3 オリジンクライアント
internal/cffunc/     CloudFront Functions ランタイム (JS エンジン)、イベントモデル、KVS
internal/sign/       署名付き URL / Cookie の検証
internal/watch/      テンプレートのホットリロード
e2e/                 統合テスト (build tag: e2e)、シナリオ別フィクスチャ
examples/            ユースケース別のサンプル一式 (e2e のフィクスチャと共用)
```

リクエストパイプラインの基本形:

```
viewer request
  → Host ヘッダで distribution 解決
  → path pattern で behavior 解決
  → allowed methods チェック
  → 署名付き URL / Cookie 検証 (trusted key groups がある場合)
  → CloudFront Functions (viewer-request)
  → cache policy / origin request policy からオリジンリクエストを構築
  → オリジン fetch (custom HTTP / S3 API)
  → エラー時: custom error responses 適用
  → CloudFront Functions (viewer-response)
  → response headers policy 適用
  → Compress (gzip / brotli)
  → CloudFront ヘッダ付与 (X-Amz-Cf-Id, X-Cache, Via, ...)
```

※ 署名検証と viewer-request function の順序は fidelity notes 対象(実 CloudFront で要確認)。

## マイルストーン

### M0: スキャフォールディング(0.5 日)

- CLI スケルトン(`localfront serve`、フラグ定義のみ)、slog、golangci-lint、GitHub Actions(unit test + lint)、Makefile。
- **テスト**: CI が回ることの確認のみ。

### M1: テンプレートローダー + 設定モデル(2–3 日)

- **スパイク**: [goformation](https://github.com/awslabs/goformation) の採用可否を判断する。intrinsic 解決の制御性・YAML 短縮形 (`!Ref` 等) の扱い・依存の重さを評価し、不適なら yaml.v3 + 自前 intrinsic 解決に切り替える。
- JSON / YAML テンプレートのロード、README 記載の intrinsic サブセット(`Ref` / `Fn::GetAtt` / `Fn::Sub` / `Fn::Join` / `Fn::FindInMap` / Parameters + `--parameter` 上書き)。
- サポートリソース型のモデル化、unknown リソースの警告付きスキップ、非対応機能(origin group 等)の明示的エラー。
- マネージドポリシーの組み込み(JSON を embed)。logical ID からの決定的 distribution ID 生成。
- **テスト(unit)**:
  - intrinsic 解決のテーブルテスト(正常系・循環参照・未解決 `Fn::ImportValue` のエラー)
  - YAML 短縮形 / JSON 完全形の等価性
  - 非対応機能を含むテンプレートが明確なエラーで落ちること
  - CDK 出力フィクスチャ(S3 Bucket 等の unknown リソース入り)が警告付きでロードできること
  - distribution ID 生成の決定性

### M2: 最小データプレーン: Host ルーティング + カスタムオリジン(1–2 日)

- HTTP サーバ起動、aliases + `<id>.cloudfront.localhost` での distribution 解決。
- default behavior のみで、カスタム HTTP オリジンへの reverse proxy(origin path、custom headers、protocol policy、タイムアウト)。
- `X-Amz-Cf-Id` / `Via` / `X-Forwarded-For` / `X-Cache: Miss from localfront` の付与。
- **テスト(unit)**: httptest をオリジンに使い、Host 解決(未知 Host は CloudFront 互換の 403)、origin path 結合、custom header 付与、ヘッダ転送を検証。
- **DoD**: テンプレート + curl で end-to-end が動く(最初の縦串)。

### M3: S3 オリジン(1–2 日)

- aws-sdk-go-v2 の S3 クライアントで `GetObject` / `HeadObject`。`<bucket>.s3[.<region>].amazonaws.com` → path-style 解決(`s3-website-*` は S3 オリジンではなくカスタムオリジン扱いであることに注意)。
- Range / 条件付きリクエスト(`Range`, `If-None-Match`, `If-Modified-Since`)の S3 パラメータへの透過、`206` / `304` の返却。
- S3 エラーのマッピング(`NoSuchKey` → 404、`AccessDenied` → 403。実 CloudFront は ListBucket 権限の有無で 403/404 が変わるが、OAC 非強制の localfront では 404 に倒す — fidelity notes に記録)。
- Default root object。
- **テスト(unit)**: ドメイン名パースのテーブルテスト、エラーマッピング。S3 API はインターフェース化してフェイクで検証。
- **テスト(integration)**: RustFS コンテナ相手に GetObject / Range / 304 のスモーク。

### M4: behavior 完全化 + ポリシーセマンティクス(2–3 日)

- path pattern マッチング: `*` / `?`、**記載順で先勝ち**(specificity ではない)、case-sensitive、default behavior へのフォールバック。
- allowed / cached methods(非許可メソッドは CloudFront 互換のステータス — fidelity notes)。
- cache policy → キャッシュキー要素 = オリジンへ転送されるヘッダ/Cookie/クエリの決定。origin request policy による追加転送。legacy `ForwardedValues` の互換解釈。
- response headers policy(CORS のプリフライト応答含む、セキュリティヘッダ、custom headers)。
- custom error responses(SPA フォールバック)、`Compress`(gzip / brotli、`Accept-Encoding` 考慮)。
- `CloudFront-Viewer-*` ヘッダの生成と、リクエスト単位の上書き(`X-Localfront-Viewer-Country: JP` 等 → 対応する `CloudFront-Viewer-*` に反映)。
- **テスト(unit)**:
  - path pattern のテーブルテスト(AWS ドキュメントの例を網羅: `images/*.jpg`、`*.gif` 等 + 順序の先勝ち)
  - cache policy / origin request policy ごとの「オリジンに届くリクエスト」のゴールデンテスト
  - CORS プリフライト(OPTIONS)と実リクエストの応答ヘッダ
  - custom error responses(オリジン 404 → 200 + 別パス本文、ErrorCachingMinTTL は無視)
  - 圧縮の有無(Content-Type / サイズ閾値 / Accept-Encoding の組み合わせ)

### M5: CloudFront Functions + KVS(3–4 日、最大のリスク)

- **JS エンジン**: QuickJS の WASM ビルドを [wazero](https://github.com/tetratelabs/wazero) 上で実行する(例: [fastschema/qjs](https://github.com/fastschema/qjs)。合わなければ QuickJS-NG の WASM ビルドを直接 wazero に載せる)。cgo 非依存なので `go install` 一発・クロスコンパイル可を維持できる。QuickJS-NG は ESM / async/await / Promise をネイティブにサポートするため、cloudfront-js 2.0 の `import cf from 'cloudfront';` も KVS の Promise も構文変換なしで扱える。
- **スパイク(M1–M2 と並行で先行着手)**: 確認項目:
  - `cloudfront` モジュール(KVS バインディング)のホスト関数ブリッジ — wazero のホスト関数で KVS アクセスを提供し、Promise 解決を WASM 境界越しに成立させる
  - リクエストごとの実行コスト計測 — インスタンスプール、wazero のコンパイルキャッシュ、テンプレートロード時の QuickJS バイトコード事前コンパイルで緩和
  - サンドボックス — std / os モジュールを公開せず、cloudfront-js に存在しない API(タイマー、ネットワーク)が見えないこと
- イベントオブジェクト(version 1.0 構造: `context` / `viewer` / `request` / `response`、ヘッダ小文字化、`multiValue`)、viewer-request / viewer-response フック、function がレスポンスを直接返すケース(リダイレクト等)。
- 実行エラー時は CloudFront 互換の 503。サイズ・実行時間制限は警告のみ(PoC ではブロックしない)。
- KVS: インメモリ実装、`ImportSource`(設定済みストアから取得)と `--kvs-seed` での投入。
- **テスト(unit)**:
  - AWS 公式サンプル関数をそのまま流す(URL rewrite、redirect、header 追加、署名 Cookie チェック系以外)
  - イベント構造のゴールデンテスト(クエリ・multiValue ヘッダ・Cookie のパース)
  - KVS get / 存在しないキーの挙動、JS 実行時例外 → 503
  - request 改変(uri 書き換え)が behavior 再評価**されない**こと(CloudFront 仕様: function はキャッシュキー前、behavior 解決後)

### M6: 署名付き URL / Cookie(1–2 日)

- `PublicKey` / `KeyGroup` の解決、behavior の trusted key groups。
- canned / custom ポリシーの検証(RSA-SHA1)、クエリパラメータ(`Expires` / `Policy` / `Signature` / `Key-Pair-Id`)と Cookie(`CloudFront-*`)の両対応。custom ポリシーの `DateLessThan` / `DateGreaterThan` / `IpAddress` 条件。
- 失敗時の CloudFront 互換 403(Missing Key 等のメッセージ)。
- **テスト(unit)**: **aws-sdk-go-v2 の URL signer をオラクルに使う** — テスト用 RSA 鍵で SDK に署名させ、localfront の検証器が受理することを確認。期限切れ・改ざん・未知 Key-Pair-Id・IP 不一致の拒否をテーブルテスト。

### M7: ホットリロード + DX 仕上げ(1 日)

- fsnotify でテンプレート / シードファイルを監視、設定スナップショットを atomic に差し替え。パース失敗時は旧設定で稼働継続 + エラーログ。
- 起動時サマリ出力(distribution 一覧、ホスト名、ポート)。
- **テスト(unit)**: 差し替えの原子性(リロード中のリクエストが新旧どちらかの一貫した設定で処理される)、壊れたテンプレートで旧設定が維持されること。

### M8: 統合テストスイート + examples(2–3 日)

下記シナリオを `e2e/` に実装し、`examples/` をフィクスチャ兼ドキュメントとして整備する。

## 統合テスト戦略

- **形態**: `go test -tags e2e ./e2e/...`。localfront は**ビルド済みバイナリをサブプロセス起動**(ブラックボックス、カバレッジは unit 側で担保)。RustFS は testcontainers-go で起動(イメージ取得不可時のフォールバックとして MinIO 互換モードを用意)。フィクスチャ投入は aws-sdk-go-v2。
- **CI**: unit は全 push、e2e は docker 利用可能な GitHub Actions ジョブで PR ごとに実行。
- **コンフォーマンス(ストレッチ、post-PoC)**: 同じシナリオ群を環境変数ゲートで**実 CloudFront に対しても実行可能**にし、エミュレータの忠実度を継続検証する。fidelity notes の解消にも使う。

## 統合テストシナリオ(現実世界のユースケース)

| # | シナリオ | 構成 | 検証内容 |
|---|---|---|---|
| 1 | **SPA ホスティング**(React/Vite 想定) | S3 オリジン + default root object + 404→/index.html(200) | `/` で index.html、`/app/route`(オブジェクトなし)が 200 で index.html、`/assets/*.js` の Content-Type、gzip/brotli 圧縮 |
| 2 | **静的アセット + API バックエンド** | default → S3、`/api/*` → カスタムオリジン(ローカル HTTP サーバ)+ `Managed-AllViewer` | path routing の先勝ち、`/api` への POST 透過と Cookie/クエリ転送、静的側は CachingOptimized でクエリ非転送、非許可メソッド拒否 |
| 3 | **CloudFront Functions: URL 正規化 + リダイレクト** | viewer-request function(`/docs` → `/docs/index.html` 補完、旧ドメインから 301)+ KVS のフィーチャーフラグ | 書き換え後 URI でオリジン取得、function 直接応答の 301、KVS 値変更(シード差し替え)で挙動が変わる |
| 4 | **会員限定コンテンツ(署名付き URL / Cookie)** | `/premium/*` に trusted key group | SDK 生成の署名付き URL で 200、署名なし/期限切れ/改ざんで 403、`/videos/*` は署名付き Cookie で複数ファイル取得(動画プレイリストの定番パターン) |
| 5 | **メディア配信(Range / 条件付き)** | S3 オリジンに動画ファイル | `Range` で 206 + 正しいバイト列、`If-None-Match` で 304 |
| 6 | **CORS + セキュリティヘッダ** | Web フォント / API への response headers policy | OPTIONS プリフライトと実リクエストの CORS ヘッダ、HSTS 等の付与 |
| 7 | **CDK 出力テンプレート** | `cdk synth` 済みフィクスチャ(Bucket / OAC / Distribution 含む) | unknown リソースを警告スキップしつつ配信が成立する(CDK 導線の保証) |
| 8 | **ホットリロード運用** | 稼働中にテンプレート編集 | behavior 追加が無停止で反映、壊れた編集では旧設定で稼働継続 |
| 9 | **マルチ distribution** | aliases の異なる 2 distribution を同一ポートで | Host ヘッダでの振り分け、`<id>.cloudfront.localhost` でのアクセス |
| 10 | **geo / デバイス依存ロジック** | viewer ヘッダ上書き + function / オリジン側で参照 | `X-Localfront-Viewer-Country: JP` がオリジンに `CloudFront-Viewer-Country: JP` として届き、function からも見える |

シナリオ 1–4 が PoC の価値を直接示すコア。5–10 は仕様の取りこぼし検出用。

## リスクと対応

| リスク | 影響 | 対応 |
|---|---|---|
| QuickJS(WASM)のホスト関数ブリッジ(KVS の Promise 解決)が複雑化する、または WASM 実行のオーバーヘッドが大きい | M5 が伸びる / レイテンシ悪化 | M1 期間中に先行スパイクで計測。バイトコード事前コンパイル + インスタンスプールで緩和。ローカル用途なので許容ラインは緩めに設定 |
| goformation が intrinsic / 短縮形を期待通り扱えない | M1 が伸びる | スパイクで早期判断。サブセット自前実装は規模的に許容範囲 |
| CloudFront 実挙動の曖昧さ(署名検証と function の順序、403/405、NoSuchKey の 403/404) | テスト期待値が誤る | fidelity notes に集約し、実 CloudFront で一括検証する日を 1 回設ける |
| RustFS の Docker イメージ / 認証情報が想定と異なる | e2e が組めない | M3 冒頭で実物確認。S3 クライアントはエンドポイント非依存なので影響は限定的 |

## 工数目安

1 人で実働 **約 2.5–3 週間**(M0–M8 合計 13–18 日)。スパイク 2 件(CFN パーサ / JS エンジン)を最初の週に消化できれば後半のブレは小さい。
