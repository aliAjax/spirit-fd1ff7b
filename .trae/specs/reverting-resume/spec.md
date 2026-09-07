# Multi-source move 反向窗口崩溃恢复（reverting 阶段自动决策）- 产品需求文档

## Overview

- **Summary**: 为 `pkg/move` 的 `--reverse-window` 多源（N:M / 1:M）迁移补齐"反向 cutover（rollback）被中断后"的重启恢复能力。重启后根据 checkpoint 的 `move_phase`、`cutover_at`、反向馈送起始位置（`binlog_position`）以及各源/目标分片上表的**实际物理形态**，自动决策"继续反向窗口 / 完成回滚 / 完成收尾 / 安全终止"，且任何分支都不得重新复制数据或重复执行 forward cutover。
- **Purpose**: 当前 `phase=reverting` 的恢复直接返回 `status.ErrOwnershipAmbiguous`（"complete it manually"，见 [runner.go](file:///Users/zhuanzmima0000/Desktop/SWE-like/projects/spirit-move-reverse-recovery/pkg/move/runner.go#L842-L844) 与 [docs/move.md](file:///Users/zhuanzmima0000/Desktop/SWE-like/projects/spirit-move-reverse-recovery/docs/move.md) 第 116 行）。生产环境中进程崩溃/重启很常见，要求人工完成半回滚状态既容易出错也无法自动化；同时 `reverting` 行当前不写反向位置（`persistPhase` 只写 phase+cutover_at），使"续做回滚"在数据面上不可能安全完成。
- **Target Users**: 通过编程方式（orchestration / Vitess 集成）运行 `spirit move --reverse-window` 的运维与自动化系统；CLI 直接使用者同等受益。

## Goals

- 重启进入 `phase=reverting` 时，自动识别回滚进行到哪一步，并：
  - **续做回滚（rollback）**：当反向 traffic switch 回调可证明**尚未执行**时，重建反向馈送、追平、补完剩余 rename、执行回调、退役目标表、收尾；
  - **完成收尾（finalize）**：当所有源表已 un-retire 且所有目标表已退役（回调可证明**已经执行**）时，仅重试幂等收尾（写 finalized、drop marker、drop checkpoint）；
  - **安全终止（terminate）**：当回调"可能已执行"无法判定、或物理状态不一致、或续做回滚所需位置缺失时，返回 `status.ErrOwnershipAmbiguous`，保留 checkpoint 与 revert marker 供人工处置，`Result()` 保留 ownership 证据。
- 决策依据完整覆盖三要素：checkpoint phase、`cutover_at`、源/目标位置；物理表形态作为 phase 的佐证与分支条件。
- 任何恢复分支不得：重新复制行、重新执行 forward cutover 回调、重复执行 reverse cutover 回调、drop 非本流程产物的表。
- 1:1 与 N:M（sharded source → sharded target）两种拓扑行为一致。

## Non-Goals

- 不改变 forward cutover、反向窗口轮询、complete-forward 的既有语义。
- 不实现反向窗口恢复时重新获取 advisory lock（沿用现有 v1 约定：并发重启同一 move 属操作员错误，见 [runner.go](file:///Users/zhuanzmima0000/Desktop/SWE-like/projects/spirit-move-reverse-recovery/pkg/move/runner.go#L860-L864)）。
- 不处理 checkpoint 表本身不可读/跨版本不兼容的恢复（现有行为：落入正常流程并安全失败）。
- 不新增 CLI 参数、不改变 checkpoint 表结构（`move_phase`/`cutover_at`/`binlog_position` 列已存在）。
- 不处理"回滚后又想重新 forward"之类的双向反复切换。

## Background & Context

反向 cutover（[reversewindow.go](file:///Users/zhuanzmima0000/Desktop/SWE-like/projects/spirit-move-reverse-recovery/pkg/move/reversewindow.go#L314-L414)）的步骤顺序：

1. 清理残留 `_revert` 表 → 2. 锁定所有（前）目标表 → 3. flush 并关闭反向馈送 → 4. **写 `phase=reverting`（ownership 边界）** → 5. 在每个源分片上 `RENAME <t>_old → <t>`（un-retire 源）→ 6. 执行调用方的 reverse cutover 回调（traffic switch，**非幂等**）→ 7. 在锁内 `RENAME <t> → <t>_revert`（退役目标表）→ 8. `finalizeReverse`：写 `phase=reverse_finalized`、drop revert marker、drop checkpoint。

关键序关系：回调（步骤 6）严格位于"所有源分片全部 un-retire（5）"之后、"任一目标表退役（7）"之前。由此得到可证明的判定：

- 任一源分片上仍存在 `<t>_old` ⇒ 步骤 5 未完成 ⇒ 回调**必然未执行**，且目标表必然都还是真名（否则状态不一致）。
- 所有目标表都已退役为 `<t>_revert` ⇒ 步骤 7 已完成 ⇒ 回调**必然已执行**。
- 所有源表已 un-retire 但仍有目标表为真名 ⇒ 回调**可能已执行**（崩溃点在步骤 6 前后或步骤 7 中途）⇒ 不可自动续做。

现有恢复入口 `maybeResumeReverseWindow`（[runner.go](file:///Users/zhuanzmima0000/Desktop/SWE-like/projects/spirit-move-reverse-recovery/pkg/move/runner.go#L830-L855)）已处理 `reverse_window`（续窗口）与 `reverse_finalized`（幂等收尾），但 `reverting` 直接报错；且 `newReverseWindow.persistPhase` 闭包写 reverting/finalized 行时 `Position` 为空（[reversewindow.go](file:///Users/zhuanzmima0000/Desktop/SWE-like/projects/spirit-move-reverse-recovery/pkg/move/reversewindow.go#L118-L120)），会覆盖 reverse_window 行中已捕获的反向馈送起始位置。

## Functional Requirements

- **FR-1（reverting 位置持久化）**: 写 `phase=reverting`（及 finalized）checkpoint 行时，必须同时持久化 `r.reversePositions`（每个目标的反向馈送起始位置，JSON 存入 `binlog_position`）与 `cutover_at`；崩溃后续做回滚所需位置不得丢失。
- **FR-2（物理状态探查）**: 恢复 `reverting` 时，从源分片（`<t>` 真名 vs `<t>_old`）与目标（`<t>` 真名 vs `<t>_revert`）探查每张逻辑表的形态；逻辑表集合从源侧 `_old`/真名表推导，并按 `SourceTables` 过滤（未指定则全部为 moved 表）。每个 (分片, 表) 必须恰为两种预期形态之一；"两者都在"或"两者都不在"为不一致。
- **FR-3（续做回滚分支）**: 当至少一个源分片仍为 `_old` 形态（⇒ 回调未执行）且所有目标均为真名形态时：
  1. 幂等补完源 un-retire（存在 `_old` 才 rename，真名已在则跳过）；
  2. 用 checkpoint 中的每目标位置重建反向馈送，目标端为源的**真名表**（非 `_old`），N:M 时按源 keyspace vindex 路由；
  3. 先不加锁追平（drain 到一次 AllChangesFlushed），再锁定目标表做最终 flush（AllChangesFlushed 为真才继续）；
  4. 重写 `phase=reverting`（含位置）→ un-retire 全部为 no-op → **执行一次** reverse cutover 回调 → 退役目标表（真名→`_revert`）→ finalize。
  - 若 checkpoint 缺少任一目标的非空位置，不得用"目标当前 head"兜底（会静默漏掉停机期间的目标写入），转为 FR-5 终止。
- **FR-4（完成收尾分支）**: 当所有源分片均为真名且所有目标均为 `_revert` 形态时：回调已执行、ownership 已确定在源侧；仅调用 `finalizeReverse`（幂等：persist finalized → drop marker → drop checkpoint），不得执行任何回调或 rename。
- **FR-5（安全终止分支）**: 以下情况返回包裹 `status.ErrOwnershipAmbiguous` 的错误，错误信息说明观察到的形态与人工处置方法，且不做任何破坏性操作、保留 marker 与 checkpoint：
  - 所有源已 un-retire 但至少一个目标仍为真名（回调可能已执行）；
  - 物理形态不一致（表缺失/两种形态同时存在/"有 `_old` 源但已有目标退役"等违反步骤序的状态）；
  - FR-3 需要重建馈送但位置缺失/为空/无法解析。
- **FR-6（幂等 rename）**: 反向 cutover 的 un-retire 与 retire 两类 rename 改为存在性感知：目标形态已达成则跳过、源形态存在才执行 rename、其余报错；正常（非恢复）路径行为不变（其前置清理与锁保证形态如预期）。恢复路径**不得**执行 `dropStaleRevertTables`（`_revert` 表在恢复中可能是本流程自己的产物）。
- **FR-7（不重复 cutover/不重复制）**: 所有非空 phase 的恢复都在 discovery/locks/copy 之前由 `maybeResumeReverseWindow` 接管并返回 `handled=true`；forward cutover 回调在任何恢复分支都不被调用；reverse cutover 回调仅在 FR-3 分支调用且至多一次。
- **FR-8（证据与状态）**: FR-3 成功后 `Result()` 报告 `DurableMutation=true`、`TerminalOwnership=ReverseFinalized`；FR-5 终止后 `Result().TerminalOwnership=Ambiguous`（沿用 `recordWorkflowError` 对 `ErrOwnershipAmbiguous` 的处理）。

## Non-Functional Requirements

- **NFR-1（安全默认）**: 任何无法证明安全的状态一律 fail-closed（终止），绝不猜测性续做；位置缺失宁停勿漏。
- **NFR-2（可观测）**: 每个恢复分支以 WARN/INFO 记录决策依据（phase、cutover_at、各分片形态摘要）；终止错误信息可操作（列出观察到的形态与人工步骤）。
- **NFR-3（测试）**: 决策矩阵为纯函数并有不依赖 MySQL 的单元测试覆盖全部分支；1:1 与 2:2 拓扑各有 MySQL 集成测试覆盖续做回滚、收尾、终止三条路径；既有反向窗口测试全部保持通过。
- **NFR-4（风格）**: 遵循仓库约定（`dbconn` 访问、`sqlescape`、testify、`wg.Go` 风格、golangci-lint v2 通过）；新代码放在 `pkg/move` 内，按现有文件切分习惯组织。

## Constraints

- **Technical**: Go 1.26+；MySQL 8.0+；不新增 checkpoint 列；反向馈送/applier 的幂等语义（REPLACE INTO / 按 PK DELETE）是"从旧位置重放"安全性的基础；rename 为原子元数据操作，同一表不可能真名/`_old` 同时存在。
- **Business**: 数据迁移工具，安全优先于便利；非默认路径/人工判断必须显式且吵闹（loud failure）。
- **Dependencies**: `pkg/checkpoint`（Record/Table）、`pkg/change`（NewAutoClient/StartFromPosition）、`pkg/dbconn`（TableLock/Exec）、`pkg/status`（ErrOwnershipAmbiguous/WorkflowResult）、`pkg/move/check`（CutoverOldName/RevertRetiredName）。

## Assumptions

- 崩溃后目标表锁随连接断开自动释放；停机期间若回调未执行，流量仍在目标侧，写入累积在目标，由重建的反向馈送在锁表前追平。
- 同一 move 不会有两个 spirit 进程并发重启（v1 不重取 advisory lock，与现有 reverse_window 恢复一致）。
- 目标侧在 move 开始前为空（既有 target_state 检查保证），因此 targets[0] 上非 `_spirit_` 的表（或 `_revert` 后缀表）均为本 move 的产物。
- `_old`/`_revert` 后缀冲突属既有 rename_safety 假设范畴（moved 表不以这些后缀命名）；极端双后缀冲突状态归入 FR-5 终止。

## Acceptance Criteria

### AC-1: reverting 行持久化反向位置
- **Type**: `rule`
- **Given**: 反向 cutover 已写入 `phase=reverting` 的 checkpoint 行
- **When**: 读取该行
- **Then**: `binlog_position` 为包含每个目标 targetKey 的非空位置 JSON，`cutover_at` 非空
- **Pass Condition**: MySQL 集成测试中通过真实 `persistPhase` 闭包写入后 `ReadLatest` 返回的 `Position` 可解析为 map 且覆盖全部目标；代码检查确认 reverting/finalized 写入路径携带 Position
- **Evidence**: 新增集成测试断言 + 代码评审

### AC-2: 决策矩阵纯函数全覆盖
- **Type**: `rule`
- **Given**: 一组 (phase, 各源分片形态, 各目标形态, 位置是否齐全) 输入
- **When**: 调用恢复决策函数
- **Then**: 输出分别为 续做回滚 / 完成收尾 / 安全终止，覆盖：全 `_old`+目标真名；部分源 un-retire；全源真名+全目标 `_revert`；全源真名+部分目标真名；表缺失；双形态并存；有 `_old` 源但目标已退役；位置缺失
- **Pass Condition**: 表驱动单元测试（无需 MySQL）断言每种输入的决策与理由，全部通过
- **Evidence**: `go test ./pkg/move/ -run <决策测试>` 输出

### AC-3: 崩溃在 un-retire 完成前 → 自动续做回滚（1:1）
- **Type**: `rule`
- **Given**: move 已进入反向窗口后被 kill（源 `_old`、目标真名、checkpoint phase=reverse_window），随后将 phase 置为 reverting（位置保留，模拟崩溃在步骤 4 之后、rename 之前），并在目标制造停机期写入
- **When**: 以相同配置重跑 spirit
- **Then**: 不复制、不调用 forward cutover；反向馈送重建并把停机期写入流回源；reverse cutover 回调执行一次；源 un-retire 并服务、目标退役为 `_revert`；marker 与 checkpoint 被清理；数据正确
- **Pass Condition**: 集成测试断言上述全部可观测事实（含停机期写入在源侧可见）
- **Evidence**: 新增 `TestMoveReverseWindowRevertingResumeCompletesRollback`（1:1）通过

### AC-4: 崩溃在 un-retire 完成前 → 自动续做回滚（2:2 sharded）
- **Type**: `rule`
- **Given**: 2:2 move 进入反向窗口后被 kill，phase 置为 reverting，两个目标分片各有停机期写入（含 insert/update/delete，跨奇偶路由）
- **When**: 重跑 spirit
- **Then**: 馈送按源 keyspace vindex 路由回正确源分片，所有源 un-retire、所有目标退役，数据逐分片正确，回调一次
- **Pass Condition**: 2:2 集成测试断言各源分片行集合与 `_old`/`_revert` 形态
- **Evidence**: 新增 N:M 集成测试通过

### AC-5: 崩溃在回调可能已执行区域 → 安全终止
- **Type**: `rule`
- **Given**: kill 后源表已手工 un-retire（真名）、目标仍为真名（模拟崩溃在步骤 6/7 之间），phase=reverting
- **When**: 重跑 spirit
- **Then**: 返回 `status.ErrOwnershipAmbiguous`；不执行任何 cutover 回调、不 rename、不 drop checkpoint/marker；`Result().TerminalOwnership == Ambiguous`
- **Pass Condition**: 更新后的歧义测试断言 errorIs、回调未被调用、checkpoint 与 marker 仍存在、Result 证据
- **Evidence**: 更新 `TestMoveReverseWindowRevertingResumeRetainsOwnershipEvidence` 类测试通过

### AC-6: 回滚物理完成仅差收尾 → 自动 finalize
- **Type**: `rule`
- **Given**: kill 后源已全部 un-retire、目标已全部退役为 `_revert`，phase=reverting（或 reverse_finalized 行）
- **When**: 重跑 spirit
- **Then**: 不执行任何回调/rename/馈送；marker 与 checkpoint 被清理；`Result()` 为 ReverseFinalized
- **Pass Condition**: 集成测试断言两个回调均未被调用、marker/checkpoint 不存在、源服务中
- **Evidence**: 新增 finalize 恢复测试通过

### AC-7: 续做回滚位置缺失 → 安全终止
- **Type**: `rule`
- **Given**: phase=reverting、源仍有 `_old`（需续做回滚）但 checkpoint `binlog_position` 为空
- **When**: 重跑 spirit
- **Then**: 返回 `status.ErrOwnershipAmbiguous`，错误信息说明位置缺失；不做任何 rename/回调/drop
- **Pass Condition**: 集成测试断言 errorIs 与表形态未变
- **Evidence**: 新增测试通过

### AC-8: 恢复路径不重复 forward cutover、不重新复制
- **Type**: `rule`
- **Given**: 任一 reverting/reverse_window/reverse_finalized 恢复场景
- **When**: 重跑 spirit
- **Then**: forward cutover 回调从不被调用；目标表行数不经历复制过程（无 copier 启动）；正常 copy-phase 恢复（phase=""）行为不变
- **Pass Condition**: 全部反向窗口/恢复集成测试中 forward 回调被设为 fail-if-called 且通过；既有 copy-resume 测试无回归
- **Evidence**: 既有 + 新增测试套件全绿

### AC-9: 恢复决策与动作的工程质量
- **Type**: `rubric`
- **Dimension**: 安全性与可维护性——决策逻辑集中、可纯函数测试；fail-closed 分支完备；日志/错误信息可操作；与既有 reverse cutover 代码复用而非分叉
- **Scale**: 1-5
- **Anchors**: 1 = 决策散落、存在猜测性续做或重复回调路径；3 = 功能正确但有分叉重复或错误信息笼统；5 = 决策矩阵单一来源、rename/收尾幂等复用、每个分支日志与错误可操作
- **Pass Threshold**: >= 4
- **Evidence**: 代码评审 + 独立审查

## Open Questions

- 无（实现中若发现拓扑状态超出本矩阵，按 NFR-1 归入终止分支并在审查中确认）。
