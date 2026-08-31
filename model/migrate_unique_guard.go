package model

import (
	"gorm.io/gorm"
	"gorm.io/gorm/migrator"
	"gorm.io/gorm/schema"
)

// guardedUniqueDialector 包装 dialector,修正 gorm v1.25.12 引入的
// MigrateColumnUnique 在 AutoMigrate 中的误判。
//
// gorm 假设 ColumnType.Unique() 只反映列级 UNIQUE 约束、不受唯一索引影响,
// 但 PostgreSQL 驱动对单列 UNIQUE 约束同样返回 unique=true。当模型字段只带
// uniqueIndex 标签(或数据库残留旧版本约束)时,AutoMigrate 会按固定命名
// uni_<table>_<column> 去删除并不存在的约束,启动直接失败
// (PostgreSQL SQLSTATE 42704)。MySQL 驱动曾有同类误判
// (information_schema columns.column_key='UNI'),已在
// gorm.io/driver/mysql v1.5.7 上游修复,故该包装只用于 SQLite 与
// PostgreSQL,MySQL 直接使用驱动自带的迁移逻辑。
//
// 这里只在规范命名的约束确实存在时才执行删除;唯一索引带来的 unique 状态
// 属于 schema 预期,保持不动。字段新增 unique 标签时的补建逻辑维持原样。
//
// 包装层同时转发 gorm 核心会做类型断言的可选接口
// (BuildIndexOptionsInterface、SavePointerDialectorInterface、
// ErrorTranslator),否则包装本身会破坏建表索引构建、嵌套事务保存点与
// 错误翻译。
type guardedUniqueDialector struct {
	gorm.Dialector
}

func guardUniqueMigration(dialector gorm.Dialector) gorm.Dialector {
	return guardedUniqueDialector{Dialector: dialector}
}

func (d guardedUniqueDialector) Migrator(db *gorm.DB) gorm.Migrator {
	return guardedUniqueMigrator{Migrator: d.Dialector.Migrator(db), db: db}
}

func (d guardedUniqueDialector) SavePoint(tx *gorm.DB, name string) error {
	return d.Dialector.(gorm.SavePointerDialectorInterface).SavePoint(tx, name)
}

func (d guardedUniqueDialector) RollbackTo(tx *gorm.DB, name string) error {
	return d.Dialector.(gorm.SavePointerDialectorInterface).RollbackTo(tx, name)
}

func (d guardedUniqueDialector) Translate(err error) error {
	if translator, ok := d.Dialector.(gorm.ErrorTranslator); ok {
		return translator.Translate(err)
	}
	return err
}

type guardedUniqueMigrator struct {
	gorm.Migrator
	db *gorm.DB
}

func (m guardedUniqueMigrator) MigrateColumnUnique(value interface{}, field *schema.Field, columnType gorm.ColumnType) error {
	unique, ok := columnType.Unique()
	if !ok || field.PrimaryKey {
		return nil
	}
	stmt := &gorm.Statement{DB: m.db}
	if err := stmt.Parse(value); err != nil {
		return err
	}
	constraint := m.db.NamingStrategy.UniqueName(stmt.Table, field.DBName)
	switch {
	case unique && !field.Unique:
		if m.Migrator.HasConstraint(value, constraint) {
			return m.Migrator.DropConstraint(value, constraint)
		}
		return nil
	case !unique && field.Unique:
		return m.Migrator.CreateConstraint(value, constraint)
	default:
		return nil
	}
}

func (m guardedUniqueMigrator) BuildIndexOptions(opts []schema.IndexOption, stmt *gorm.Statement) []interface{} {
	return m.Migrator.(migrator.BuildIndexOptionsInterface).BuildIndexOptions(opts, stmt)
}
