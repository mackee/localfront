# 設計判断の記録

実装中に確定した技術選定・設計判断。経緯ごと残す。

## e2e のコンテナ駆動: ory/dockertest 採用（2026-06 確定）

統合テスト（`e2e/`）で RustFS コンテナを起動する方法。当初計画は testcontainers-go だったが **ory/dockertest** を採用した。

経緯:
- 最初の実装では「依存フットプリントを増やしたくない」という理由で `docker` CLI 直叩き（`os/exec`）を選んだが、readiness 判定・cleanup・ポート解決をすべて手書きする羽目になり、ストア初期化中の 503 レースで一度不安定になった。
- フットプリントの懸念は過大評価だった: e2e は `//go:build e2e` かつ `cmd/localfront` の import グラフ外なので、コンテナ系ライブラリの依存は **`go install .../cmd/localfront@latest` では取得もビルドもされない**。残コストは「ルート `go.mod` の require に載る」「`go test ./...` で取得される」だけ。
- そこで ory/dockertest に切り替えた。testcontainers-go より軽量で、`pool.Retry()`(readiness リトライ)・`pool.Purge()` / `resource.Expire()`(cleanup)・`GetHostPort()`(ポート解決)が手書きロジックを置き換える。Docker daemon API を直接叩くので `docker` CLI が PATH に無くても動く。
- 依存の置き場所は**同一モジュール**（ルート `go.mod`）。ネストモジュールにすると `go test ./...` が e2e を再帰しなくなり CI/ローカル実行が二重管理になるため避けた。
- イメージは `LOCALFRONT_E2E_S3_IMAGE` で差し替え可能（MinIO 互換へのフォールバック）。Docker 不在時は `dockertest.NewPool` / `Ping` 失敗で `t.Skip`。
- フィクスチャ投入（バケット作成・オブジェクト PUT）は S3 クライアントと同じ aws-sdk-go-v2 SigV4 署名器を流用。空ボディは `Content-Length: 0` 明示、書き込みは 503 リトライ付き。

## CFN パーサ: goformation 不採用、yaml.v3 + 自前実装（2026-06 確定）

- [awslabs/goformation](https://github.com/awslabs/goformation) は **2024-10-17 にアーカイブ済み**（最終リリース v7.14.9, 2024-04）。
- 設計面でも不適: intrinsic 解決がパース時に固定で結合しており（`ProcessJSON()`）、未解決 intrinsic が型付き struct の unmarshal を壊す既知バグあり（Issue #302）。localfront が必要とする「解決タイミングと値の完全な制御」が取れない。
- 全 ~600 リソース型の生成コードを抱えるためコンパイル面も重い。
- 参考実装として [aws-cloudformation/rain](https://github.com/aws-cloudformation/rain) の `cft/parse`（短縮形タグの正規化）は有用だが、モジュールごと import すると AWS SDK 一式が付いてくるため、正規化パス（~100行）を自前実装した。
- 実装: `internal/cfntmpl/yaml.go` — yaml.Node を歩いて `!Ref` 等の短縮形を長形式の単一キー map に正規化し、plain な Go 値ツリーに変換。JSON テンプレートには短縮形が存在しないので同一コードパスで処理できる。

## intrinsic 解決のアーキテクチャ

- `cfntmpl` はリソースのセマンティクスを知らない。`Ref` / `Fn::GetAtt` が何に評価されるかは `RefValuer` インターフェース（`internal/config/refs.go` が実装）に委譲。
- 参照値（distribution ID 等）は **logical ID と型だけから決定的に導出**し、プロパティには依存しない。このため参照解決に循環が発生し得ない（CFN の循環参照エラー再現は不要になった）。
- 解決できない参照（未対応リソース型への Ref/GetAtt）は **エラーにせず placeholder 文字列（`localfront:unresolved:<LogicalID>`）に置換 + 警告**。config 構築時に「実際に使われるプロパティ」（オリジンドメイン、ポリシー ID 等）に placeholder が残っていたらそこでエラーにする。これにより:
  - CDK テンプレートの ACM 証明書 ARN など「受理するが無視する」プロパティ内の未解決参照は許容される（README の互換方針）。
  - 実害のある未解決だけが明確なエラーになる。

## AWS::S3::Bucket の参照限定サポート

CDK 出力ではオリジンの `DomainName` が `Fn::GetAtt [Bucket, RegionalDomainName]` になるのが通例。Bucket リソース自体はスキップしつつ、参照だけは実値に解決する:

- `Ref` → `BucketName` プロパティ（リテラル時）、なければ logical ID から導出（小文字化・サニタイズ）
- `GetAtt RegionalDomainName` → `<name>.s3.us-east-1.amazonaws.com` 等

bucket 名が intrinsic で組み立てられている場合はフォールバック名になる点に注意（実用上は CDK が物理名をリテラルで埋めるか、名前未指定でフォールバックが安定 ID として機能する）。

## 擬似パラメータの固定値

| 擬似パラメータ | 値 |
| --- | --- |
| `AWS::Region` | `us-east-1` |
| `AWS::AccountId` | `123456789012` |
| `AWS::Partition` | `aws` |
| `AWS::URLSuffix` | `amazonaws.com` |
| `AWS::StackName` | `localfront` |

生成 ARN（distribution / function / KVS）も AccountId `123456789012` を使う。

## ID 生成

- distribution: `"E" + base36(sha256("distribution:" + logicalID))[0:13]`（CloudFront 風の E + 13 文字）
- public key: `K` プレフィックス、OAC: `E` プレフィックス（salt 違い）
- ポリシー / KeyGroup / KVS: sha256 から UUID 形式
- すべて logical ID のみから決定的。再起動・マシン間で安定。

## マネージドポリシーのデータ化

`internal/config/managed_policies.json` に AWS ドキュメント由来の定義を embed（cache 7 / origin request 8 / response headers 5）。データは概ね CFN プロパティ形だが、リスト項目が API 形（`{"Items": [...]}`）で書かれている箇所があるため、デコード側の `StringList` 型が素のリストと `{Items}` の両方を受理する。

既知の曖昧点（M4 で要再確認）:
- `Managed-SimpleCORS` / `Managed-CORS-and-SecurityHeadersPolicy` の `AccessControlAllowHeaders/Methods` はドキュメントに明記がなく補完値（simple CORS は `Access-Control-Allow-Origin: *` のみ送出が正）。
- `Managed-Amplify` / `Managed-Elemental-MediaPackage` の brotli は docs 文言に従い false。

## JS エンジン: fastschema/qjs（QuickJS-NG WASM on wazero）採用（2026-06 スパイク完了）

判定: **viable-with-caveats**。`github.com/fastschema/qjs` v0.0.6（依存は wazero のみ、cgo 非依存）で cloudfront-js 2.0 に必要な全機能（ES2020+、ESM、async handler、Promise の WASM 境界越え解決、Go 実装のホスト関数）を確認済み。1000 並列リクエストのストレスで 100% 正答。検証コードは `/tmp/localfront-spike-qjs/`（step1–step6、step6 が統合パターンのリファレンス実装）。M5 実装時に `internal/cffunc` へ移植する。

### 計測値（Apple Silicon）

- `qjs.New()` 初回: ~355ms（プロセスごと1回の wasm コンパイル。`Option.CacheDir` でディスクキャッシュ可）、2回目以降 ~2.1ms
- 関数ロード（eval）: ~160–250µs、バイトコード事前コンパイル可
- ウォームな1リクエスト実行（event in → handler+KVS await → JSON out）: **~70–85µs**。プール必須ではないがランタイムは goroutine-safe でないため、関数ごとのプール（または mutex 保護）を採用する

### 確定した統合パターン（M5 の実装仕様）

1. 関数ごとに N 個のランタイムをテンプレートロード時に準備（プール）
2. `qjs.New(qjs.Option{CWD: 空のtempディレクトリ})` — **WASI がデフォルトでプロセス CWD を `/` にマウントし JS から実ファイルが読み書きできてしまう**ため、必ず空ディレクトリに閉じ込める
3. `ctx.SetFunc("__kvsGet", ...)` で同期ホスト関数として KVS を公開し、JS 側 shim で `Promise.resolve().then(() => __kvsGet(key))` にラップ（async ホスト関数の goroutine からの settle は禁止 — 下記）
4. quickjs-libc 由来の `std` / `os` / `setTimeout` / `setInterval` をプレリュードで undefined に潰し cloudfront-js 2.0 のサーフェスに合わせる
5. `ctx.Load("cloudfront", shim)` で bare specifier `'cloudfront'` を解決（import の書き換え不要）。`import` を含むコードは `export default handler` を付けてモジュール評価、含まないコードはグローバル評価して `handler` を取得
6. **境界を越えるのは文字列だけ**: JS 側トランポリン `__runHandler = (eventJson) => Promise.resolve(__handler(JSON.parse(eventJson))).then(r => JSON.stringify(r))` を介して JSON 文字列で受け渡す

### 発見した上流バグ（要ワークアラウンド、upstream 報告候補）

- `Value.JSONStringify()` に use-after-free（`qjswasm/helpers.c:583` が Go 読み出し前に `JS_FreeCString`）。`Value.String()` は安全 → トランポリンで JS 側 stringify する（上記 6 がワークアラウンドを兼ねる）。再現コード: `/tmp/localfront-spike-qjs/repro-jsonstringify-bug/`
- README の async 例どおりに goroutine から `this.Promise().Resolve()` すると `Await()` 中の wazero モジュールに並行進入してメモリ破壊。settle は同期に行う
- `Await()` は promise 値を消費する（後から `Free()` すると double-free で panic）
- handler の戻り値は event オブジェクトの子であることが多く、event を先に Free すると壊れる（トランポリンで回避）
