# Portfolio Risk Engine (Go)

本地组合风险计量与压力测试引擎：以本地行情与持仓数据，在**历史模拟法**与
**参数法**两种口径下计量 VaR（风险价值）与 ES（期望损失），支持并发重算、
行情增量更新与压力情景叠加。核心设计目标是：**同输入必同结论、异常必留痕、
并发无串扰、增量等于全量**。

## 1. 包结构

| 文件 | 职责 |
| --- | --- |
| `risk/types.go` | 持仓、行情、收益序列、问题（Issue）、组合、结果等核心类型 |
| `risk/market.go` | 并发安全的行情/历史存储；不可变快照；增量更新去重与乱序拒绝 |
| `risk/portfolio.go` | 持仓校验、权重/暴露归一化、覆盖率计算、缺失/过期/非法区分 |
| `risk/methods.go` | 历史模拟法、参数法、日期对齐、分位数与正态逆 CDF |
| `risk/engine.go` | 组合注册、单次/批量并发计算、输入摘要哈希 |
| `risk/stress.go` | 压力情景叠加（相对/绝对冲击） |
| `risk/snapshot.go` | 确定性输入哈希（SHA-256） |

## 2. 风险模型口径

两种方法均以 **NAV 分数**表达损失，VaR/ES 返回正数表示亏损。

### 2.1 历史模拟法

1. 各资产收益序列按**共同交易日取交集**对齐（缺失日期不补零、不静默跳过）；
2. 组合 P&L 序列：`pl_t = Σ_i w_i · r_{i,t}`，权重在保留资产上归一化至毛暴露 1；
3. VaR 为 P&L 分布在 `α = 1−c` 分位数的相反数，采用 **R-7 / numpy linear
   线性插值分位数**：`h=(n−1)α`，`q = x_⌊h⌋ + (h−⌊h⌋)(x_⌊h⌋+1 − x_⌊h⌋)`；
4. ES 为 `pl ≤ q` 部分均值的相反数（尾损均值），故恒有 `ES ≥ VaR`。

### 2.2 参数法

假设收益联合正态：

- 均值向量与样本协方差矩阵在共同交易日上估计（样本方差，分母 n−1）；
- `VaR_c = z_c · σ_p − μ_p`，其中 `z_c = Φ⁻¹(c)`（Acklam 有理近似，误差 ≈ 1.15e-9）；
- `ES_c = σ_p · φ(z_c)/α − μ_p`，`φ` 为标准正态密度。

### 2.3 可复现性

结果携带 `InputsHash`：对组合名、方法、置信水平、每个保留资产的归一化权重、
行情价格与**完整收益序列**做确定性 SHA-256 摘要（资产按 ID 排序）。
相同输入（无论装载路径）哈希相同，数值相同；测试断言这两条性质。

## 3. 装载与归一化规则

- 每个持仓**二选一**提供 `Weight`（毛 NAV 分数，可正可负）或 `Exposure`
  （金额，按当前价换算为 `Exposure/Price` 单位数）；同时提供或都不提供 → 拒绝；
- 重复资产、非有限数值 → 硬拒绝（`LoadError`，聚合全部原因，不静默丢弃）；
- 通过校验的资产再做行情覆盖检查；被降级剔除的资产，剩余权重**按毛暴露重新
  归一化为 1**（保留多空符号），`Portfolio.Coverage` = 保留毛权重 / 总毛权重；
- 若所有资产都被降级剔除，返回硬拒绝，避免把空组合误当全投资结果。

## 4. 异常行情处理策略

所有异常都落到可区分的 `IssueKind`，出现在错误或 `Result.Issues` 中：

| Kind | 触发条件 | 处理 |
| --- | --- | --- |
| `invalid_holding` | 空 ID、重复持仓、权重与暴露同时给 | **硬拒绝** |
| `invalid_price` | 价格 ≤ 0、NaN、±Inf（装载、更新、情景三处一致） | **硬拒绝/该资产剔除** |
| `invalid_returns` | 收益非有限、长度与日期不符、日期非严格递增 | **硬拒绝该序列** |
| `missing_quote` | 无行情 | 降级：剔除该资产，记 issue |
| `stale_quote` | 行情年龄超过 `StaleWindow`（默认 24h） | 降级，issue 含实际年龄与窗口 |
| `no_history` / `short_history` | 无序列 / 观测数少于 `MinHistory` | 降级 |
| `duplicate_update` | 增量更新序号与该资产已应用序号相同 | 幂等拒绝，不改数据 |
| `out_of_order_update` | 序号小于已应用序号（或非正） | 拒绝，不改数据 |
| `unknown_asset` | 空资产 ID、未注册组合等 | 硬拒绝 |

过期详情示例：
`stale_quote [B]: quote age 240h0m0s exceeds window 168h0m0s; asset excluded (degraded)`。

## 5. 并发计算与数据更新一致性规则

1. **不可变快照**：`MarketStore.View()` 深拷贝出一份 `Snapshot`；计算只读快照，
   永远不持有 store 锁，进行中的计算不受增量更新影响。
2. **批量共享视图**：`ComputeMany` 对整批请求只取一次快照，再并发计算；结果按
   请求顺序回填，goroutine 之间无任何共享可变状态。
3. **存储层锁**：store 用 `sync.RWMutex` 保护 map 与序号；engine 用独立锁保护
   组合定义。
4. **增量更新幂等与防乱序**：更新携带每资产单调 `Sequence`；相同序号视为重复
   投递（幂等返回 `duplicate_update`），更小序号视为乱序（`out_of_order_update`），
   均不落库，因此重放/乱序不会重复计价，价格与结论不漂移。
5. **增量 ≡ 全量**：风险是最终快照的纯函数（资产排序固定、浮点运算顺序固定），
   故「分批增量更新到状态 S」与「一次性全量装载到状态 S」产出逐位相同的
   VaR/ES 与相同 `InputsHash`（测试以 1e-15 容差校验）。
6. **输入摘要不含投递元数据**：`Sequence` 是传输序号而非风险输入，刻意不进哈希。

## 6. 压力情景

`ApplyScenario(portfolio, snapshot, scenario)` 返回情景下的假设 P&L：

- `Shock{UseRelative:true, Relative:-0.10}` 表示 −10% 乘法冲击；
- `Shock{Absolute:5}` 表示价格绝对位移 5 个计价单位；同一资产多条冲击按
  乘法/加法合成，不会被重复计价；
- 冲击后价格 ≤ 0 或非有限 → `invalid_price`，该资产贡献被剔除并记 issue，
  绝不按零计价；未持有的资产上的冲击无 P&L 影响。

## 7. 验证方法

```bash
go vet ./...
go test ./... -count=1 -v          # 全部 18 个用例，含输入快照与判定依据日志
go test -race ./... -count=1       # 竞态检测（并发重算 / 更新读混合）
```

测试矩阵（每个用例日志都打印**输入快照**与**判定依据**）：

- `TestHistoricalVaRHandComputed` / `TestNormalQuantileKnownValues` /
  `TestParametricVaRSingleAsset`：手工可核对数值（R-7 分位、z(0.99)、正态 ES）；
- `TestAlignHistoriesIntersection`：日期交集对齐，不补零；
- `TestNormalize*` / `TestLoadInvalidPriceRejected`：基本归一化、覆盖率、
  `missing_quote` 与 `stale_quote` 可区分、短历史、重复持仓、暴露换算；
- `TestConcurrentRecomputeNoRaceNoCrosstalk`：200 goroutine × 6 请求，
  逐位比对基准，配合 `-race`；
- `TestConcurrentUpdatesAndReads`：更新线程与 16×200 次批量重算交错执行；
- `TestIncrementalEqualsFullRecompute`：3 组合 × 2 口径 × 2 置信水平，
  增量与全量数值（1e-15）与哈希一致；
- `TestDuplicateAndOutOfOrderUpdates`：重复/乱序/非法价格更新被区分拒绝，
  价格保持不变；
- `TestStressScenarioOverlay`：混合冲击手算 P&L；冲击致负价 → `invalid_price`。

## 8. 快速上手

```go
store := risk.NewMarketStore(7*24*time.Hour, 30)
store.LoadQuotes(quotes)      // 返回 invalid_price 等问题，调用方必须处理
store.LoadHistory(hist)
e := risk.NewEngine(store)
_ = e.Register("eq", []risk.Position{{Asset: "A", Weight: 0.6}, {Asset: "B", Weight: 0.4}})

res, err := e.Compute("eq", risk.MethodHistorical, 0.99)
// res.VaR, res.ES（正损失分数）; res.Degraded / res.Issues; res.InputsHash

_ = store.Apply([]risk.QuoteUpdate{{Quote: newQuote, Sequence: 101}}) // 幂等+防乱序
```
