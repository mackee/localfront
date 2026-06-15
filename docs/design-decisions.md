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

## examples の検証: runn（aqua 管理の CLI）（2026-06 確定）

`examples/` の各テンプレートが「動くドキュメント」であることを保証するため、宣言的シナリオランナー [runn](https://github.com/k1LoW/runn) で実バイナリ越しに検証する。

- **入手方法**: **aqua 管理の CLI**（`aqua.yaml` に `k1LoW/runn`）。go.mod には入れない — runn は依存ツリーが重く、ランタイムにもユニットテストにも不要なため。`runn` バイナリは PATH 経由で使う。
- **配置**: シナリオは `examples/<name>/scenario.yaml`（テンプレートの隣 = 自己文書化）。`runners.req.endpoint: ${LF_ENDPOINT}` で対象を切り替え、`runn run` 単体でも実行可能。
- **ホストルーティング**: localfront は Host ヘッダで distribution を解決する。runn の HTTP runner は `headers.Host` を `req.Host` に反映する（`http.go`）ので、各ステップで `Host: <alias>` を指定すればローカルの 127.0.0.1:port 宛でも正しい distribution に届く。
- **オーケストレーション（2 方式）**:
  - **カスタムオリジンの例**（functions / cors-security）: 依存が軽いので e2e Go テスト（`e2e/examples_runn_test.go`、`e2e` タグ）が httptest の echo オリジン + localfront を起動し、`LF_ENDPOINT` を渡して `runn run scenario.yaml` を実行（CWD をシナリオのディレクトリにして runn の `read:parent` scope を回避）。**Docker 不要**。オリジンのポートだけ環境依存なのでテンプレートの `HTTPPort: 3000` を実ポートに置換する（テンプレートの構造・Function・ポリシーはそのまま検証対象）。
  - **S3 を要する例**（spa-hosting / static-and-api）: 各例に `compose.yaml`（RustFS + mc シード、static-and-api は `http-echo` バックエンドも）を同梱し、`scripts/verify-example.sh`（`task verify:spa` / `verify:static-and-api` が起動）が compose up → シード → localfront をホストで起動 → `runn run` → teardown する。README の「依存は Docker、localfront はホスト」という使い方そのままを検証する。
- **runn が無いとき**: Go テストは `t.Skip`。
- **タスクランナー**: Makefile を廃し [Taskfile](https://taskfile.dev)（`Taskfile.yml`、`go-task/task` も aqua 管理）に統一。`task build/test/e2e/lint/verify:examples`。CI の e2e ジョブは aqua で task/runn を入れ、`go test -tags e2e` と `task verify:examples` の両方を実行する。

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

既知の曖昧点（M4 で再確認済み）:
- `Managed-SimpleCORS` / `Managed-CORS-and-SecurityHeadersPolicy` の `AccessControlAllowHeaders/Methods` はドキュメントに明記がなく補完値（simple CORS は `Access-Control-Allow-Origin: *` のみ送出が正）。M4 の CORS 適用では「Origin ヘッダのあるリクエストにのみ CORS ヘッダを付与」「`*` 指定は無条件、リスト指定は一致時に echo + `Vary: Origin`」とした。
- `Managed-Amplify` / `Managed-Elemental-MediaPackage` の brotli は docs 文言に従い false。

## M4: behavior セマンティクスの実装判断（2026-06 確定）

`internal/behavior` パッケージに、HTTP トランスポートから独立した純粋関数群として実装（単体テスト容易性のため）。データプレーンがこれをパイプラインに配線する。

- **path pattern マッチング** (`MatchPath`): `*`(0文字以上)・`?`(1文字)。両ワイルドカードとも `/` を跨ぐ（シェル glob とは異なる）。先頭 `/` は pattern/path 双方から除去して比較するため `images/*` と `/images/*` は等価。case-sensitive。バックトラッキング付き反復マッチャ（再帰なし）。
- **behavior 選択** (`Select`): CacheBehaviors を**テンプレート記載順で先勝ち**評価（specificity ではない）。マッチしなければ default behavior。
- **オリジンへ届くもの** (`BuildOriginRequest`): ヘッダ/Cookie/クエリそれぞれ「cache policy の選択 ∪ origin request policy の選択」。cache policy のヘッダは none/whitelist のみ、ORP は none/whitelist/allViewer/allViewerAndWhitelistCloudFront/allExcept。viewer 由来の `CloudFront-*` ヘッダは破棄し、合成済みプールの値のみを policy 選択時に転送。`X-Localfront-*` 制御ヘッダは転送しない。
- **常時転送ヘッダ**: `Range` と条件付きヘッダ（`If-*`）は policy に関係なくオリジンへ転送（CloudFront の Range/条件付き対応に合わせ、M3 の挙動を維持）。`X-Forwarded-For` も policy 非依存で viewer 値に追記。
- **Accept-Encoding 正規化**: cache policy の gzip/brotli フラグが有効なら、viewer が受理する有効エンコーディングのみに正規化（gzip 先頭）。両方無効なら policy が明示的に転送しない限り削除。
- **圧縮** (`ChooseEncoding` + オンザフライ圧縮): `Compress=true` かつ viewer の Accept-Encoding に gzip/br があり、compressible Content-Type・`Content-Length` が 1,000〜10,000,000・未エンコード・`no-transform` でない・ステータス 200・GET のときのみ。**gzip を brotli より優先**（決定性 + 標準ライブラリ）。brotli は `github.com/andybalholm/brotli`（pure Go, cgo 非依存で `go install` 一発を維持）。圧縮は 10MB 上限内なのでバッファリングして `Content-Length` を再計算、`Vary: Accept-Encoding` を付与。
- **response headers policy** (`ApplyResponseHeaders`): remove → custom → security → CORS の順。各ヘッダの `Override=false` はオリジン既存値を優先。CORS は Origin ヘッダのあるリクエストにのみ付与。
- **custom error responses**: オリジンが実際に返したステータス（4xx/5xx）にのみ適用（オリジン接続失敗の合成 502 には適用しない）。`ResponsePagePath` 指定時はそのパスで behavior を再選択してページを取得し、`ResponseCode` で返す（SPA フォールバックの典型）。未指定時はステータスのみ書き換えてオリジン本文を維持。`ErrorCachingMinTTL` は PoC が無キャッシュのため無視。再取得は 1 段のみ（無限ループ防止）。

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

### M5 実装（2026-06 確定）

スパイクの統合パターンを `internal/cffunc` に移植した。確定した実装判断:

- **プール**: qjs に `Pool.Close()` が無くホットリロードでランタイムがリークするため、`internal/cffunc/pool.go` に**クローズ可能な自前プール**を実装。再利用時に `rt.Call("QJS_UpdateStackTop", rt.Raw())` でスタックトップを呼び出し goroutine に再アンカー（`Runtime.Raw()` が公開されているため外部から呼べる）。関数ごとに 1 プール（デフォルト 8 ランタイム）。
- **ロード戦略**: コードに `import` 行があれば ESM として評価し（無ければ `export default handler` を付加）、無ければグローバル評価して `globalThis.handler` を取得。どちらも `globalThis.__handler` に格納し、JSON in/out のトランポリン `__run` 経由で呼ぶ（`JSONStringify` の use-after-free と event/result ライフタイム問題を回避）。
- **サンドボックス**: `qjs.Option.CWD` を関数ごとの空 tempdir にして WASI のファイルアクセスを封じる。prelude で `std`/`os`/`setTimeout`/`setInterval`/`print` を undefined 化し、`console.log/error/warn/info` をホスト関数 `__log` 経由（slog debug）にルーティング。
- **`cloudfront` モジュール**: prelude でグローバル `cf`（と `__makeKvsHandle`）を定義し、`ctx.Load("cloudfront", "export default globalThis.cf")` で bare specifier を解決。KVS は同期ホスト関数 `__kvsGet`/`__kvsExists` を JS 側で `Promise.resolve().then(...)` にラップ。`kvs.get(key, {format:'json'})` の JSON パースに対応。
- **イベントモデル** (`event.go`): version 1.0 構造。ヘッダ名は小文字化、繰り返しは `multiValue`、Cookie はヘッダから分離。合成済み `CloudFront-*` viewer ヘッダをイベントの request.headers にマージ（function から見える — シナリオ 10）。
- **結果判定**: viewer-response は常に response。viewer-request は `statusCode` を持てば response（短絡）、無ければ request。
- **エラー → 503**: JS 例外・エンジンエラーは CloudFront 互換 503。サイズ/実行時間の上限は PoC では非強制（警告のみ）。
- **KVS**: インメモリ。`ImportSource`（S3 ARN をストアから取得）と `--kvs-seed <store>=<file>`（Name または logical ID で一致、ImportSource を上書き）。シードは AWS bulk 形式 `{"data":[{key,value}]}` とフラット `{key:value}` の両方を受理。
- **パイプライン位置**: viewer-request は behavior 解決・メソッドチェック後、`BuildOriginRequest` の前（**behavior 再評価なし** — fidelity note 8）。viewer-response はオリジン取得・custom error responses の後、response headers policy の前。function 追加ヘッダのオリジン到達は origin request policy のフィルタ対象（fidelity note 13）。

## M6: 署名付き URL / Cookie 検証（2026-06 確定）

`internal/sign` に実装。trusted key group を持つ behavior でのみ検証する。

- **署名方式**: RSA-PKCS1v15 + SHA1。aws-sdk-go-v2 の CloudFront signer と同一（`signer.Sign(rand, sha1(jsonPolicy), crypto.SHA1)`）。base64 は CloudFront の URL 安全置換（`+→-`, `=→_`, `/→~`）を逆変換してデコード。
- **テストのオラクル**: `github.com/aws/aws-sdk-go-v2/feature/cloudfront/sign` をテスト依存に追加し、テスト用 RSA 鍵で SDK に署名させて localfront の検証器が受理することを確認（canned / custom / 期限切れ / 改ざん / 未知 Key-Pair-Id / IP 不一致 / DateGreaterThan / 署名 Cookie）。SDK は `cmd/localfront` の import グラフ外（テストのみ）。
- **canned policy**: 署名対象 policy が Resource URL を埋め込むため、localfront は `<scheme>://<Host><path>` を **https → http の順**で再構成して署名一致を試す（ローカルは平文 HTTP のため scheme が曖昧 — fidelity note 15）。一致後に `Expires` を検証。policy JSON は SDK のエンコード（`json.Encoder` の HTML エスケープ無効・空白除去・固定フィールド順）をバイト単位で再現。
- **custom policy**: `Policy` パラメータをデコードした生バイトに対して署名検証し、その後 `DateLessThan` / `DateGreaterThan` / `IpAddress`（CIDR、`/32` 補完）/ `Resource`（path 部分のワイルドカード照合）を検証。
- **資格情報の取得**: クエリ（`Expires`/`Signature`/`Key-Pair-Id` または `Policy`/...）を優先し、無ければ `CloudFront-*` Cookie にフォールバック。
- **Key-Pair-Id**: localfront が logical ID から生成する PublicKey ID（`K...`）を使う（fidelity note 16）。公開鍵 PEM は PKIX/SPKI と PKCS#1 の両方を受理。スナップショット構築時にパースし、失敗鍵は警告してスキップ（その鍵での検証は常に拒否）。
- **パイプライン位置**: メソッドチェック後、viewer-request function の**前**（fidelity note 1）。失敗は CloudFront 互換 403。

## M7: ホットリロード（2026-06 確定）

`internal/watch`（fsnotify ラッパ）+ `cmd/localfront/reload.go`（オーケストレーション）。

- **監視対象**: テンプレート + `--kvs-seed` ファイル。エディタは rename でファイルを差し替え inode 監視が外れるため、**親ディレクトリを監視**してイベントを対象ファイルにフィルタ。200ms デバウンスで保存時のイベントバーストを 1 回のリロードに集約。
- **原子性**: `dataplane.Server` は設定+関数+鍵を単一の `atomic.Pointer[snapshot]` で保持し `Swap` で差し替え。実行中リクエストは開始時のスナップショットを保持（`TestSwapConfig_AtomicUnderLoad` を `-race` で検証）。
- **エラー時継続**: リロードはまず config をロードし関数をコンパイルしてから `Swap` する。途中で失敗したら何も差し替えず旧設定を維持（`buildFunctions` は失敗時に作りかけの関数を Close、`reloader.reload` は config ロード/コンパイルエラーで早期 return）。
- **旧世代関数の解放**: `Swap` 後、旧世代の compiled functions（QuickJS ランタイム）は **30 秒の猶予**後に Close（`time.AfterFunc`）。直前に開始した実行中リクエストがドレインできる。リーク上限は 1 世代分。
- **起動サマリ**: data plane URL、テンプレート（hot reload 表示）、distribution ごとに ID/logical ID とホスト名一覧を出力。
