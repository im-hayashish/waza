# microsoft/waza のフォーク

## 概要

これは [microsoft/waza](https://github.com/microsoft/waza) のフォークである。
Waza は AI エージェントの skill を評価する CLI で、eval spec をエージェントエンジンに対して実行し、その結果を採点する。

このフォークが存在する唯一の理由は、`claude-code` 実行エンジンを追加することである。
これは Claude Code CLI（`claude`）をヘッドレスで駆動し、GitHub Copilot の premium-request クォータではなく Claude サブスクリプションに対して評価を実行させる。
それ以外はできる限り upstream に近い状態を保つ。

## upstream への追従（定期的に実施）

upstream の更新は速いため、このフォークは定期的に追従する必要がある。
マージを安価に保つため、フォークの差分は可能な限り小さく保つ。

- `upstream` リモートは microsoft/waza を指す（`git remote add upstream https://github.com/microsoft/waza.git`）
- 追従は `git fetch upstream` の後、`upstream/main` を merge または rebase する
- upstream との唯一の意図的な差分は `claude-code` エンジンとその最小限の配線だけであり、マージがそれ以外を持ち込んだら疑う

### upstream に対するフォークの簡潔な差分説明

| ファイル | 変更内容 |
|---|---|
| `internal/execution/claude_code.go` | フォーク専用 — エンジン本体 |
| `internal/execution/claude_stream.go` | フォーク専用 — stream-json パーサ |
| `internal/execution/claude_grade_bridge.go` | フォーク専用 — prompt/LLM-judge の grade ツール用 in-process MCP ブリッジ |
| `internal/execution/claude_transcript.go` | フォーク専用 — transcript 系グレーダ向けに `ExecutionResponse.Events` を合成 |
| `internal/execution/claude_code_test.go`, `claude_stream_test.go`, `testdata/claude_stream_*.jsonl` | フォーク専用 — テスト |
| `cmd/waza/cmd_run.go` | エンジンの switch に `case "claude-code":` を1つ追加 |
| `internal/orchestration/runner.go` | `claude-code` のときだけ `ExecutionRequest.SourceDir` を設定（後述） |
| `schemas/config.schema.json`, `schemas/eval.schema.json` | executor/engine の enum に `claude-code` を追加 |
| `internal/validation/schema_test.go` | enum 値のバリデーションテスト |
| `go.mod` | `github.com/mark3labs/mcp-go` を direct 依存へ昇格（既にモジュールグラフ内のため `go.sum` は不変） |
| docs（`site/…`, `README.md`） | `claude-code` executor のドキュメント |

共有された更新頻度の高い upstream ファイル、特に `internal/execution/copilot.go` にはフォークの変更を入れない。
upstream とバイト単位で同一に保つ。
エンジンは自己完結しており、copilot の `getSkillDirs` を共有せず独自の `claudeSkillDirs` を持つため、upstream が copilot をリファクタしても衝突しない。

## `claude-code` エンジンの実装メモ

- エントリポイントは `execution.NewClaudeCodeEngine(modelID)` で、`cmd/waza/cmd_run.go` で登録し、copilot / mock と同じ `AgentEngine` / `WorkspaceKeeper` 契約を実装する
- 実行はタスクごとに `claude -p --output-format stream-json …` サブプロセスを1つ、タスク専用の一時ワークスペースで起動し、`claude_stream.go` が stream-json 出力を `ExecutionResponse`（最終出力・ツール呼び出し・skill 呼び出し・usage・session id）へマッピングする
- 認証は環境変数 `CLAUDE_CODE_OAUTH_TOKEN`、または `claude setup-token` が保存した認証情報（`~/.claude/.credentials.json`、`CLAUDE_CONFIG_DIR` を尊重）のいずれかで行い、`Initialize` はどちらも無い場合のみ失敗する。`ANTHROPIC_API_KEY` は子プロセスの環境から除去し、使用量が従量課金 API ではなくサブスクリプション枠に載るようにする
- モデル名は `normalizeClaudeModel` が copilot 形式のドット表記（`claude-haiku-4.5`）を CLI が期待するハイフン形式（`claude-haiku-4-5`）へ書き換え、`haiku` のような素のエイリアスはそのまま通すため、`copilot-sdk` 向けの spec が無改変で動く
- skill 呼び出しは `Skill` という名前の `tool_use` ブロックとして現れ、skill id は `skill` 入力キーに入る（例: `{"skill":"greeting","args":"Bob"}`。`claude_stream_real_skill.jsonl` フィクスチャに対応）。`claude_stream.go` がこれを `SkillInvocations` に記録し、`skill_invocation` グレーダが動作する
- ツール引数は `decodeToolArgs` が mapstructure を使い、非標準の引数を `ToolCallArgs.Extra`（`tool_calls`/`tool_constraint` の引数マッチャが参照）へ入れ、Claude の `file_path` キーを正準の `Path` フィールドへエイリアスする
- instruction ファイル（`req.Instructions`）は一時ファイルに書き出し、インラインの `--append-system-prompt` ではなく `--append-system-prompt-file` で渡すことで、大きな instruction が単一引数上限 `MAX_ARG_STRLEN`（Linux で約 128 KiB）を超えて `E2BIG` で exec がクラッシュするのを防ぐ。skill 自体は `<workspace>/.claude/skills/<name>` にマテリアライズしてネイティブに利用可能にし、本文は注入しない
- MCP サーバ（`req.MCPServers`）は一時 JSON ファイルに書き出し、`--mcp-config <file> --strict-mcp-config` で読み込む（`buildMCPConfig` が copilot の stdio/http 設定を CLI の `mcpServers` 形式へ変換する）
- in-process ツールコールバック / grade ツール（`req.Tools`）は、CLI がサブプロセスで Go の `Handler` クロージャを直接呼べないため、`claude_grade_bridge.go` が in-process の MCP-over-HTTP サーバ（`mark3labs/mcp-go`、stateless）を立てて各 `req.Tools` を公開し、呼び出しを元の Go ハンドラへ転送する。これを `--mcp-config` に `waza-graders` キーで追加し、system-prompt ブロック（`buildGradeToolGuidance`）が judge にツール呼び出しで結果を記録するよう指示する（CLI 名前空間形式は `mcp__waza-graders__<tool>`）。グレーダ側のコードは無改変で、クロージャが捕捉する `Passes`/`Failures` は copilot と同様に populate される
- マルチターンではフォローアップと responder ループが前ターンの `SessionID` を渡し直し、エンジンが `--resume` で再開する。単発の `EphemeralSession` 実行（judge）は代わりに `--no-session-persistence` を渡すが、CLI はそれでも `~/.claude/projects` 配下にプロジェクトごとの auto-memory ディレクトリを作るため、`Shutdown` がそれらをワークスペース単位で purge する（`purgeWorkspaceProjects`。セッションファイルを残さない ephemeral 実行もカバーする）
- transcript は、CLI が Copilot SDK の session-event ログを返さないため、`claude_transcript.go` がパース済みストリームから `ExecutionResponse.Events` を合成する（user メッセージ、raw 引数と結果テキストを持つ tool start/complete のペア、最終 assistant メッセージ）。使うのは `internal/models` 自身も生成する exported な SDK data 型で、`runner.go` がこれを `copilotevents.ToSDK` → `transcript.BuildFromSessionEvents` / `models.FilterToolCalls` を通じてラウンドトリップさせるため、`transcript` / `tool_calls` を読む `inline_script` グレーダが実データを見られる。共有の transcript モデルは fork しない
- `FirstEventTimeout` / `CancelOnSkillInvocation` は、キャンセル可能な run context とストリームフック（`parseClaudeStream` が `onFirstEvent` / `onSkillInvoked` を発火）で対応する。ストリーム出力の無いセッション開始ハングは kill し、専用の `errClaudeFirstEventTimeout` で報告する。skill 呼び出しは run をキャンセルしてサブプロセスを kill し、run-context の cause 経由で期待された早期終了として扱う（`resp.Success = true`、エラーはクリア。両フラグをセットする trigger 精度 eval が必要とする形）
- `SourceDir` のゲーティングは重要で、`runner.go` は executor が `claude-code` のときだけ `ExecutionRequest.SourceDir` を設定する。エンジンは `skill_directories` が省略されたとき、これを起点に親方向へ辿って skill を探す。copilot エンジンは populate された `SourceDir` を自身の skill ベースディレクトリと解釈して cwd ベースの探索を失い、無条件に設定すると `TestRun_Skills_RunAtRoot*` テストが退行するため、このゲートは外さない

## copilot-sdk との差分（parity を既定）

これは個人フォークであり、唯一の仕事は同じ eval を Claude サブスクリプションで実行することである。
設計上の契約は parity で、`copilot-sdk` executor ができることは `claude-code`もできる。
spec はどちらの executor で走るかを意識する必要がなく、eval 作者が `copilot-sdk`を選ばざるを得ない状況も作らない。
このセクションは動くものを列挙せず、`claude-code` が`copilot-sdk` と一致しない箇所だけを追跡する。
載っているものは受け入れる境界ではなく、直すべきバグとして扱う。

現時点で既知の挙動差はない。
`claude-code` が `copilot-sdk` と異なる挙動を示す spec を見つけたら、それはバグとして扱い、直すまでの間だけここに追記する。

次の2つの `ExecutionRequest` オプションは、欠落ではなくアーキテクチャ上の差異のため意図的に配線しない。
挙動を無理に作って修正しない。

- `SuppressSkillBody` — copilot はこれがセットされない限り対象 skill の本文を system メッセージへ注入するが、`claude-code` は本文を注入せず、skill を `<workspace>/.claude/skills/<name>` にマテリアライズして CLI のネイティブ progressive disclosure（まず名前と説明、本文は呼び出し時のみ）で提示する。これは既に `SuppressSkillBody=true` の形であり、これをセットする trigger 精度 eval は正しく動く。`=false`（本文を先出し）を honor するのは本文注入を意味し、実際の Claude サブスクリプションの提示方法に反する。`NoSkills`（完全 off）は honor する
- `PermissionHandler` — eval フローのどこでもカスタム（非 allow-all）ハンドラは設定されず、既定の allow-all は `--permission-mode bypassPermissions` と一致するため、カスタムハンドラが設定され始めない限り対応対象はない

実装メモ（ギャップではなく、知っておくべきこと）:

- `DeleteSession` は未実装で responder の `Close` は no-op になるが、削除対象の CLI セッションファイルは `Shutdown` の `purgeWorkspaceProjects` が purge する
- grade ツールのガイダンス（`buildGradeToolGuidance`）は任意の `req.Tools` に対して注入する。CLI が MCP ツールを `mcp__waza-graders__<tool>` と名前空間化するためで、文言はグレーダと responder の両方で読めるよう中立にする

## このリポジトリでの作業

- `go.mod` に宣言された Go ツールチェーンが必要で、変更を提案する前に `gofmt` で整形し `go test ./...` を実行する
- スキーマに触れる場合は `config.schema.json` と `eval.schema.json` を揃え、`site/src/content/docs/reference/schema-changes.md` にメモを追記する
- upstream との差分を最小に保つ変更を優先する
