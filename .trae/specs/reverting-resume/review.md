# Multi-source move 反向窗口崩溃恢复 - Independent Review

- [x] CP-R1: reverting/finalized checkpoint 行持久化反向位置与 cutover_at
  - **Type**: `rule`
  - **Covers**: AC-1, TR-1.1, FR-1
  - **Evidence**: `newReverseWindow.persistPhase`（reversewindow.go L121-L142）每个 phase 行写入序列化的 `reversePositions` 与 `CutoverAt`；`TestPersistPhaseCarriesReversePositions` PASS（审查人独立重跑）。

- [x] CP-R2: 决策矩阵纯函数覆盖全部物理形态分支且 fail-closed
  - **Type**: `rule`
  - **Covers**: AC-2, TR-2.1, FR-2/FR-5
  - **Evidence**: `decideRevertAction`（revertresume.go）全局聚合 old/revert 标志，序关系推导与反向 cutover 步骤一致；`TestDecideRevertAction`（13 子例）+ 多表全局序关系测试 PASS；每个 ambiguous 决策带非空 reason。

- [x] CP-R3: 1:1 续做回滚端到端正确
  - **Type**: `rule`
  - **Covers**: AC-3, AC-8, FR-3, FR-7
  - **Evidence**: `TestMoveReverseWindowRevertingResumeCompletesRollback` PASS（停机写入 id=97 流回源真名表；forward 回调 fail-if-called 未触发；reverse 回调计数器==1；目标退役 `_revert`；marker/checkpoint 清理；Result=ReverseFinalized）。权威 flush 在目标锁内 + AllChangesFlushed 门。

- [x] CP-R4: 2:2 sharded 续做回滚端到端正确
  - **Type**: `rule`
  - **Covers**: AC-4, FR-3
  - **Evidence**: `TestMoveReverseWindowRevertingResumeNM` PASS（两目标停机写入经 vindex 路由回正确源分片；全源 un-retire、全目标退役；回调==1）。

- [x] CP-R5: ambiguous 区域安全终止且零副作用
  - **Type**: `rule`
  - **Covers**: AC-5, FR-5, FR-8
  - **Evidence**: `TestMoveReverseWindowRevertingResumeRetainsOwnershipEvidence` PASS（`errors.Is(ErrOwnershipAmbiguous)`；两回调未触发；源/目标形态逐表未变；checkpoint 保留；TerminalOwnership=Ambiguous）；歧义与位置解析失败均以 `%w` 包裹 ErrOwnershipAmbiguous。

- [x] CP-R6: 物理完成仅差收尾时幂等 finalize
  - **Type**: `rule`
  - **Covers**: AC-6, FR-4
  - **Evidence**: `TestMoveReverseWindowRevertingResumeFinalizes` PASS（无 feed/无回调；marker+checkpoint 清理；Result=ReverseFinalized）；finalize 顺序由 `TestFinalizeReversePersistsOwnershipBeforeCleanup` 钉住。

- [x] CP-R7: 位置缺失时安全终止且形态不变
  - **Type**: `rule`
  - **Covers**: AC-7, FR-3 末段
  - **Evidence**: `TestMoveReverseWindowRevertingResumeMissingPositions` PASS（errorIs Ambiguous、错误信息点名 position、源保持 `_old`/目标真名、checkpoint 保留）；`buildFeed` 对每目标缺位置第二道硬失败，无 head 兜底。

- [x] CP-R8: 既有反向窗口/回滚/copy-resume 行为无回归
  - **Type**: `rule`
  - **Covers**: AC-8, TR-3.1
  - **Evidence**: 非空 phase 在 discovery/locks/copy 前被 `maybeResumeReverseWindow` 接管，copy 路径另有 phase 非空硬失败兜底；审查人独立重跑 `go build ./...`、gofmt、go vet 干净；全套 `TestMoveReverseWindow|TestFinalize|TestReverseCutover|TestPersistPhase|TestDecideRevert` ok 8.451s；pkg/move 全包 ok 23.8s；pkg/change ok 42.4s；pkg/checkpoint、pkg/status 均 ok。

- [x] CP-U1: 实现质量
  - **Type**: `rubric`
  - **Covers**: AC-9, FR-6, NFR-1/2/4
  - **Scale**: 1-5 — **得分 4（>= 4 阈值，pass）**
  - **Anchors**: 决策矩阵单一来源（纯函数+步序注释）；幂等 rename/finalize 被正常与 resume 路径复用（resuming 开关，无分叉 cutover）；ambiguous reason 可操作；日志含决策与形态摘要（加固后）。扣分项均为 advisory 小项（waitCaughtUp 无上界超时等）。
  - **Evidence**: 独立审查报告 R1。

## Review History

### Review R1
- **Result**: `pass`
- **Evidence**: 全新上下文独立只读审查（general_purpose_task），对照 AC-1..AC-9 逐条核验代码与不变量，并在 docker MySQL 8.0.45（GTID-on，:8033）独立重跑全部测试；0 个 actionable 发现，6 条 advisory 建议（F-1..F-6）。
- **Advisory 处置（复审前加固）**:
  - F-1 已采纳：`discoverRevertLogicalTables` 改为源分片 + 目标双侧名称并集，显式 SourceTables 全量预埋种子——表在两侧缺失也会被探查并判 formAbsent → ambiguous（防止外部干预导致整表漏探）。
  - F-2 已采纳：`renameOldToRealIfPresent`/`renameRealToRevertIfPresent` 的 both/neither 形态错误统一包裹 `status.ErrOwnershipAmbiguous`，ownership 证据准确。
  - F-4 已采纳：恢复决策 WARN 日志增加 `forms` 形态摘要（`revertSnapshot.summary()`）。
  - F-5 已采纳：续做回滚测试回调改为 atomic 计数器断言恰好一次；位置缺失测试断言错误信息点名 position。
  - F-3（waitCaughtUp 无上界超时）/ F-6（finalize resume 不回填 positions）为纯可用性/无安全影响项，ctx 可取消且锁内权威 flush 为正确性门，保持现状。
  - 加固后重跑：目标测试集 ok 8.120s；pkg/move 全包 ok 21.057s（move）/ 2.278s（check）；gofmt/vet 干净。
- **Blocked By**: 无。
- **环境限制（记录）**: 仅验证 GTID-on MySQL 8.0.45；file+position 坐标经同一 NewAutoClient 分类机制，与本改动正交。golangci-lint 因模块代理网络受限未能运行，以 gofmt + go vet 替代。
