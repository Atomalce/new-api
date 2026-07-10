# Database Guidelines

> new-api 数据库访问规范(GORM 用法、事务与行锁、迁移、查询模式)。内部项目精简版。

本文与仓库根 `AGENTS.md` 的 "Database compatibility" 一节为同一套约束,冲突时以 `AGENTS.md` 为准。

---

## Overview

- ORM:GORM v2(`gorm.io/gorm v1.25.2`),两个全局句柄:
  - `model.DB` — 主库,由 `SQL_DSN` 决定,必须同时兼容 **SQLite / MySQL >= 5.7.8 / PostgreSQL >= 9.6**(初始化见 `model/main.go` 的 `chooseDB`/`InitDB`)。
  - `model.LOG_DB` — 日志库,由 `LOG_SQL_DSN` 决定,可额外为 ClickHouse;未配置时复用 `DB`(`model/main.go` 的 `InitLogDB`)。
- 分层架构 Router → Controller → Service → Model,**数据库访问集中在 `model/` 包内**,以模型方法或包级函数暴露(如 `model.GetUserById`、`(*Token).Insert`)。存量 controller 中有少量直接 `model.DB` 调用,属历史遗留,新代码不要模仿。
- 方言判断统一用 `common.UsingMainDatabase(...)` / `common.UsingLogDatabase(...)`(`common/database.go`),不要自己比对 DSN 字符串。
- 模型 JSON 字段(text 列存 JSON)的序列化必须走 `common.Marshal`/`common.Unmarshal`(`common/json.go`),禁止直接调用 `encoding/json`(AGENTS.md 硬性规定)。

---

## Models & Naming Conventions

- 模型 struct 定义在 `model/` 下,一实体一文件(`model/token.go`、`model/channel.go` 等)。表名走 GORM 默认蛇形复数(`Token` → `tokens`);仅在必须时用 `TableName()` 覆盖(如 `model/checkin.go`、`model/perf_metric.go`)。
- 主键交给 GORM 生成,**禁止**在 tag 或 DDL 里写 `AUTO_INCREMENT` / `SERIAL`。
- 列 tag 惯例(摘自 `model/token.go`):

```go
type Token struct {
    Id          int            `json:"id"`
    UserId      int            `json:"user_id" gorm:"index"`
    Key         string         `json:"key" gorm:"type:varchar(128);uniqueIndex"`
    ExpiredTime int64          `json:"expired_time" gorm:"bigint;default:-1"`
    ModelLimits string         `json:"model_limits" gorm:"type:text"`
    DeletedAt   gorm.DeletedAt `gorm:"index"`
}
```

- 软删除用 `gorm.DeletedAt`;管理端需要看全量时显式 `Unscoped()`(见 `model/user.go` 的 `GetAllUsers`)。
- **禁止** `gorm:"default:true"` 这类布尔默认值 tag:MySQL/PostgreSQL 归一化差异会导致 AutoMigrate 每次重启重复发 `ALTER TABLE`。业务默认值放请求归一化、构造函数或 hook 里(AGENTS.md)。
- `group`、`key` 是保留字列,拼 SQL 时必须用 `model/main.go` 的 `commonGroupCol` / `commonKeyCol`(日志库用 `logGroupCol` / `logKeyCol`);布尔字面量用 `commonTrueVal` / `commonFalseVal`。

---

## Migrations

没有独立迁移工具,迁移在**启动时**执行,且只在 master 节点跑(`common.IsMasterNode`,见 `model/main.go` 的 `InitDB`)。

- **加新表**:定义 struct 后,把它加进 `model/main.go` 的 `migrateDB()` 与 `migrateDBFast()` 两处 `AutoMigrate` 列表。
- **加新列**:AutoMigrate 自动补列,通常无需手写。
- **改列类型**:AutoMigrate 不会做,须手写幂等迁移函数并在 `migrateDB()` 开头调用。范式见 `model/main.go` 的 `migrateTokenModelLimitsToText`:先查 `information_schema` 确认当前类型(已迁移则直接返回),再按方言分支发 `ALTER TABLE`(PG 用 `ALTER COLUMN ... TYPE`,MySQL 用 `MODIFY COLUMN`),SQLite 因类型亲和性直接跳过。
- **SQLite 限制**:只支持 `ALTER TABLE ... ADD COLUMN`,不支持 `ALTER COLUMN`。需要精确 DDL 时用 `PRAGMA table_info` 探测已有列再逐列补齐(见 `model/main.go` 的 `ensureSubscriptionPlanTableSQLite`)。
- 每个迁移函数必须**幂等**(先检查再执行),因为每次重启都会跑。
- 日志库为 ClickHouse 时不走 AutoMigrate,用手写 `CREATE TABLE IF NOT EXISTS` + TTL 同步(`model/main.go` 的 `migrateClickHouseLogDB`)。

---

## Transactions & Row Locking

- 统一用闭包式事务 `DB.Transaction(func(tx *gorm.DB) error { ... })`,返回 error 即回滚;事务内所有读写都必须走 `tx` 而不是 `DB`。
- `SELECT ... FOR UPDATE` 行锁**必须**用 `model/locking.go` 的 `lockForUpdate(tx)`:它对 MySQL/PostgreSQL 追加 `clause.Locking{Strength: "UPDATE"}`,对 SQLite 跳过(SQLite 无该语法)。禁止 GORM v1 的 `tx.Set("gorm:query_option", "FOR UPDATE")`(GORM v2 静默忽略,等于没加锁),也禁止在调用点自己写 `clause.Locking`。
- 因为 SQLite 拿不到行锁,关键状态翻转必须叠加 **CAS**(条件更新 + 检查 `RowsAffected`)。标准写法摘自 `model/redemption.go` 的 `Redeem`:

```go
err = DB.Transaction(func(tx *gorm.DB) error {
    err := lockForUpdate(tx).Where(keyCol+" = ?", key).First(redemption).Error
    if err != nil {
        return errors.New("无效的兑换码")
    }
    // ...业务校验...
    result := tx.Model(&Redemption{}).
        Where("id = ? AND status = ?", redemption.Id, common.RedemptionCodeStatusEnabled).
        Updates(map[string]interface{}{
            "redeemed_time": common.GetTimestamp(),
            "status":        common.RedemptionCodeStatusUsed,
            "used_user_id":  userId,
        })
    if result.Error != nil {
        return result.Error
    }
    if result.RowsAffected == 0 {
        return errors.New("该兑换码已被使用")
    }
    return tx.Model(&User{}).Where("id = ?", userId).
        Update("quota", gorm.Expr("quota + ?", redemption.Quota)).Error
})
```

- 跨函数复用事务时,以 `tx *gorm.DB` 参数传入、`tx == nil` 时自建事务(见 `model/ability.go` 的 `UpdateAbilities`、`model/user.go` 的 `InsertWithTx`)。

---

## Query Patterns

**推荐写法**(均有真实出处):

- **分页**:入参用 `common.PageInfo`(`common/page_info.go`),`Limit(pageInfo.GetPageSize()).Offset(pageInfo.GetStartIdx())`;count 与 find 放同一事务保证一致(`model/user.go` 的 `GetAllUsers`)。
- **敏感列**:返回给前端的查询显式 `Omit("password", "access_token")` 或 `Select` 白名单列。
- **原子自增**:计数/额度累加一律 `gorm.Expr`,禁止读出来加完再写回:
  `DB.Model(&Channel{}).Where("id = ?", id).Update("used_quota", gorm.Expr("used_quota + ?", quota))`(`model/channel.go:864`)。
- **Upsert**:用 `clause.OnConflict`——批量忽略冲突 `DoNothing: true`(`model/ability.go`),按列累加 `DoUpdates`(`model/perf_metric.go`)。
- **批量插入**:先 `lo.Chunk(items, 50)` 分块再 `Create(&chunk)`,避免超长 SQL(`model/ability.go` 的 `AddAbilities`)。
- **跨表查询**:用 `Joins("left join channels on abilities.channel_id = channels.id")`(`model/ability.go:37`)。本项目模型无关联定义、不用 `Preload`;禁止在循环里逐行发查询(N+1)。
- **NotFound 判断**:`errors.Is(err, gorm.ErrRecordNotFound)`,或复用 `model/utils.go` 的 `RecordExist(err)`。
- **日志表**:logs 相关读写只走 `LOG_DB`,查询条件带 `logs.` 表前缀(`model/log.go` 的 `GetAllLogs`);删除大批日志按 limit 分批(`DeleteOldLogBatch`),ClickHouse 单独分支。
- **高频计数写入**:user/token/channel quota 这类热点累加,在 `common.BatchUpdateEnabled` 时经内存聚合器 `addNewRecord` 定期批量落库(`model/utils.go`),新的热点计数应接入该机制而非每请求直写,参考 `model/user.go` 的 `IncreaseUserQuota`。
- **Redis 缓存回填**:cache miss 落库后,用 `gopool.Go` 异步回写缓存,条件用 `shouldUpdateRedis(fromDB, err)`(`model/utils.go`、`model/user.go` 的 `GetUserSetting`)。
- **quota 数值换算**:任何 float/decimal → int 的 quota 转换必须用 `common/quota_math.go` 的 `QuotaFromFloat` / `QuotaRound` / `QuotaFromDecimal`(计费路径用 `*Checked` 变体),禁止裸 `int(...)` 强转(AGENTS.md)。

**Raw SQL 准入条件**:优先 GORM 链式方法;确实绕不开时(方言 DDL、ClickHouse、`information_schema` 探测)必须:

1. 用 `common.UsingMainDatabase(...)` / `UsingLogDatabase(...)` 分支,且**每种支持的库都有有效路径**;
2. 标识符引用按方言处理(PG `"col"`,MySQL/SQLite 反引号),保留字列用 `commonGroupCol` / `commonKeyCol`;
3. 值一律 `?` 占位符参数化,禁止把用户输入拼进 SQL 字符串。

范例:`model/log.go` 的 `DeleteOldLogBatch`(ClickHouse 走 `ALTER TABLE ... DELETE`,其余库走 GORM `Delete`)。

---

## Forbidden Patterns

| 禁止 | 原因 / 替代 |
|---|---|
| `tx.Set("gorm:query_option", "FOR UPDATE")` | GORM v2 静默忽略,不加锁。用 `lockForUpdate(tx)`(`model/locking.go`) |
| 调用点手写 `clause.Locking{Strength: "UPDATE"}` | 绕过 SQLite 分支。统一走 `lockForUpdate(tx)` |
| 读-改-写更新计数/额度 | 并发丢更新。用 `gorm.Expr("quota + ?", n)` 或批量聚合器 |
| 无方言分支的裸 SQL、DB 专属函数/操作符、无 TEXT 回退的 JSON 列类型 | 三库必须同时可用(AGENTS.md) |
| `fmt.Sprintf` 拼用户输入进 SQL | SQL 注入。用 `?` 参数化 |
| SQLite 上 `ALTER COLUMN` | 语法不支持。只用 `ADD COLUMN`,范式见 `ensureSubscriptionPlanTableSQLite` |
| tag/DDL 里写 `AUTO_INCREMENT` / `SERIAL` | 主键交给 GORM |
| `gorm:"default:true"` 布尔默认值 | AutoMigrate 反复 ALTER。默认值放代码层 |
| 循环内逐行查询(N+1) | 用 `Joins` / `IN` / 批量查询 |
| 用 `DB` 查写 logs 表 | logs 只在 `LOG_DB`(可能是独立库/ClickHouse) |
| 主库(`SQL_DSN`)配 ClickHouse | `chooseDB` 直接报错,ClickHouse 仅限日志库 |
| quota 换算裸 `int(...)` / `int(math.Round(...))` 强转 | 溢出可产生负扣费。用 `common/quota_math.go` 帮助函数 |
| 业务代码直接 `encoding/json` 序列化模型 JSON 字段 | 用 `common.Marshal` / `common.Unmarshal`(AGENTS.md) |

---

## References

- `AGENTS.md` — Backend Rules(JSON 包装、三库兼容、行锁、quota 换算的上游硬性规范)
- `model/main.go` — 连接初始化、方言列名、全部迁移范式
- `model/locking.go` — `lockForUpdate` 实现与注释
- `model/redemption.go` — 事务 + 行锁 + CAS 标准范例
- `common/quota_math.go` — quota 数值换算帮助函数
