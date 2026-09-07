# Multi-source move 反向窗口崩溃恢复（reverting 阶段自动决策）- 实施计划

## Task 1: reverting/finalized checkpoint 行持久化反向位置
- **Status**: `completed`
- **Priority**: high
- **Depends On**: None
- **Description**:
  - 修改 [reversewindow.go](file:///Users/zhuanzmima0000/Desktop/SWE-like/projects/spirit-move-reverse-recovery/pkg/move/reversewindow.go) 中 `newReverseWindow` 的 `persistPhase` 闭包：每个 phase 行都序列化 `r.reversePositions` 写入 `checkpoint.Record.Position`，并携带 `CutoverAt`；positions 为空时 Position 留空（行为同今）。
- **Acceptance Criteria Addressed**: AC-1, FR-1
- **Test Requirements**:
  - `rule` TR-1.1: 真实 `persistPhase` 闭包写入后 `ReadLatest` 返回 reverting 行、Position 可解析且覆盖 targetKey、cutover_at 非零 — `TestPersistPhaseCarriesReversePositions` 通过。
  - `rule` TR-1.2: finalized 行字段一致 — 由 `TestFinalizeReversePersistsOwnershipBeforeCleanup` 与 finalize 集成测试间接覆盖，通过。
- **Completion Evidence**:
  - 代码：reversewindow.go `newReverseWindow.persistPhase`（L121-L142）。
  - 测试：`TestPersistPhaseCarriesReversePositions` PASS（MySQL 8.0.45 GTID-on，:8033）。

## Task 2: reverting 恢复决策纯函数与物理形态探查
- **Status**: `completed`
- **Priority**: high
- **Depends On**: None
- **Description**:
  - 新文件 [revertresume.go](file:///Users/zhuanzmima0000/Desktop/SWE-like/projects/spirit-move-reverse-recovery/pkg/move/revertresume.go)：`tableForm`（absent/real/old/revert/both）、`revertSnapshot`、`decideRevertAction` 纯函数（complete-rollback / finalize / ambiguous 三分支 + operator-facing reason）、`tablePresence`（一次 information_schema 查询两个候选名）、`presenceForm`、`discoverRevertLogicalTables`（源侧真名+`_old` 去后缀并集，按 SourceTables 过滤）、`buildRevertSnapshot`、`reversePositionsComplete`。
  - 单元测试：[revertresume_test.go](file:///Users/zhuanzmima0000/Desktop/SWE-like/projects/spirit-move-reverse-recovery/pkg/move/revertresume_test.go) 表驱动 13 例 + 多表全局序关系 1 例。
- **Acceptance Criteria Addressed**: AC-2, AC-9, FR-2
- **Test Requirements**:
  - `rule` TR-2.1: `TestDecideRevertAction`（13 子例）+ `TestDecideRevertActionMultiTableGlobalSwitchProof` 全部 PASS，无需 MySQL。
  - `rule` TR-2.2: 形态探查 helper 由四条 reverting 集成测试（1:1/NM/ambiguous/finalize/missing-pos）真实 MySQL 覆盖，全绿。
- **Completion Evidence**:
  - `go test ./pkg/move/ -run 'TestDecideRevertAction'` PASS（0.84s）；集成测试见 Task 5 证据。

## Task 3: reverseCutover 幂等化与 resume 模式
- **Status**: `completed`
- **Priority**: high
- **Depends On**: None
- **Description**:
  - `reverseWindow.resuming` 字段；resume 时跳过 `dropStaleRevertTables`。
  - un-retire 改用 `renameOldToRealIfPresent`（真名已在则跳过，形态不符报错）；retire 改用 `renameRealToRevertIfPresent`（锁内，已退役则跳过）。
  - `buildFeed` 支持 `resuming` 时反向目标为源**真名**表。
  - 新增 `waitCaughtUp`：feed Start 后显式 Flush 轮询、两次连续 AllChangesFlushed 再锁表。
  - **附带修复**：`ReverseFeed` 客户端设置 `DDLFilterSchema`/`DDLFilterTables` 限定在被监控的（前目标）schema/表，避免同实例拓扑下源侧 spirit 自身 rename 重放时误判 schema-change 致命。
- **Acceptance Criteria Addressed**: FR-3（机制）, FR-6, AC-9
- **Test Requirements**:
  - `rule` TR-3.1: 既有 `TestMoveReverseWindow*`、`TestFinalize*`、`TestReverseCutover*` 全部继续 PASS。
  - `rule` TR-3.2: 幂等 rename 由续做回滚/finalize 集成测试覆盖（重复形态跳过），PASS。
- **Completion Evidence**:
  - 全套 `TestMoveReverseWindow|TestFinalize|TestReverseCutover` PASS（8.0s）；pkg/move 全包测试 PASS（22.8s）。

## Task 4: resumeReverting 接线（phase=reverting 三分支）
- **Status**: `completed`
- **Priority**: high
- **Depends On**: Task 1, Task 2, Task 3
- **Description**:
  - `(r *Runner).resumeReverting`（revertresume.go）：解析位置/cutoverAt → 发现逻辑表 → 物理快照 → decideRevertAction → finalize（`newReverseWindow(r).finalizeReverse`）/ completeRevertRollback（un-retire 源 → 重建 logic-name 表清单 → buildFeed(resuming) → Start → waitCaughtUp → 幂等 reverseCutover，包在 `status.Do(ReverseWindow)`）/ ambiguous（包裹 `status.ErrOwnershipAmbiguous`，不动任何表）。
  - `maybeResumeReverseWindow` 的 `case phaseReverting` 改为接管调用并更新注释。
- **Acceptance Criteria Addressed**: FR-3, FR-4, FR-5, FR-7, FR-8, AC-3..AC-8
- **Test Requirements**:
  - `rule` TR-4.1/4.2: 由 Task 5 五个集成测试覆盖（续做 1:1、续做 2:2、ambiguous、finalize、位置缺失），全绿；所有非空 phase 均 `handled=true`（forward 回调在各测试中 fail-if-called 且未被调用）。
- **Completion Evidence**:
  - 代码：revertresume.go `resumeReverting`/`completeRevertRollback`/`renameOldToRealIfPresent`/`renameRealToRevertIfPresent`；runner.go `maybeResumeReverseWindow`。
  - 集成测试全部 PASS（见 Task 5）。

## Task 5: 集成测试（1:1 与 2:2 的续做/收尾/终止）
- **Status**: `completed`
- **Priority**: high
- **Depends On**: Task 4
- **Description**:
  - `TestMoveReverseWindowRevertingResumeCompletesRollback`（1:1 续做，停机期写入 id=97 流回源，回调一次，无 forward cutover，产物清理，Result=ReverseFinalized）。
  - `TestMoveReverseWindowRevertingResumeNM`（2:2 续做，两目标停机期写入按 vindex 路由回源分片，全源 un-retire、全目标退役）。
  - 更新 `TestMoveReverseWindowRevertingResumeRetainsOwnershipEvidence`（源已 un-retire/目标仍真名 → ambiguous，无副作用，Result=Ambiguous）。
  - `TestMoveReverseWindowRevertingResumeFinalizes`（全 rename 完成仅差收尾 → 无回调/无馈送，marker+checkpoint 清理）。
  - `TestMoveReverseWindowRevertingResumeMissingPositions`（位置缺失 → ambiguous，形态不变）。
  - `TestPersistPhaseCarriesReversePositions`（真实闭包写位置）。
- **Acceptance Criteria Addressed**: AC-1, AC-3, AC-4, AC-5, AC-6, AC-7, AC-8
- **Test Requirements**:
  - `rule` TR-5.1: 六测试在 docker MySQL 8.0.45（GTID-on，127.0.0.1:8033，tsandbox）下全部 PASS。
  - `rule` TR-5.2: 无 GTID 拓扑未在本环境覆盖（环境仅有 GTID-on 8.0.45；位置坐标按既有 NewAutoClient 机制自动选择，与本改动正交）— 记录为环境限制。
- **Completion Evidence**:
  - `MYSQL_DSN="tsandbox:msandbox@tcp(127.0.0.1:8033)/test" go test ./pkg/move/ -run 'TestMoveReverseWindow|TestFinalize|TestReverseCutover|TestPersistPhase|TestDecideRevert'` → ok 8.013s。
  - `go test ./pkg/move/... ./pkg/checkpoint/... ./pkg/status/... ./pkg/change/...` → 全部 ok（move 22.8s / check 2.7s / checkpoint 2.0s / status 1.1s / change 42.4s）。

## Task 6: 文档更新、全量测试与 lint
- **Status**: `completed`
- **Priority**: medium
- **Depends On**: Task 5
- **Description**:
  - 更新 [docs/move.md](file:///Users/zhuanzmima0000/Desktop/SWE-like/projects/spirit-move-reverse-recovery/docs/move.md) Resume 条目为三分支决策描述（续窗口/续做回滚/收尾/ambiguous 终止）。
  - `go build ./...` 通过；`gofmt -l pkg/move/` 无输出；`go vet ./pkg/move/ ./pkg/checkpoint/` 无输出。golangci-lint 因模块代理网络超时无法安装，以 gofmt+go vet 替代。
- **Acceptance Criteria Addressed**: AC-8, AC-9, NFR-3, NFR-4
- **Test Requirements**:
  - `rule` TR-6.1: build/vet/fmt 零错误 — 已验证。
  - `rule` TR-6.2: move/checkpoint/status/change 包全量测试通过、copy-resume 无回归 — 已验证。
  - `rubric` TR-6.3: 文档准确性自评分 5/5：四种结果（窗口续做、回滚续做、幂等收尾、ambiguous 终止）及决策依据（phase/cutover_at/位置/物理形态）均有描述，无旧"must be completed manually"表述。
- **Completion Evidence**:
  - 见上述命令输出；docs/move.md L116-L120。
