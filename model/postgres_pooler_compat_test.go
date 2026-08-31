package model

import (
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

type poolerCompatWidget struct {
	ID    uint `gorm:"primaryKey"`
	Quota int64
}

func (poolerCompatWidget) TableName() string { return "pg_pooler_compat_widgets" }

// TestPostgresPoolerCompatStartup 保护 PostgreSQL 启动路径:为兼容事务池代理而
// 关闭 GORM 预处理语句后,driver/postgres 的 migrator GetRows 会注入
// pgx.QueryExecModeSimpleProtocol 参数,使占位符编号与实参错位,导致
// ColumnTypes(以及已有表的 AutoMigrate、启动时的钱包 schema 校验)报
// "insufficient arguments"(go-gorm/gorm#7675)。openPostgres 通过自建连接池
// 绕过该注入,此测试锁定修复后的启动行为。
func TestPostgresPoolerCompatStartup(t *testing.T) {
	dsn := strings.TrimSpace(os.Getenv("TEST_POSTGRES_DSN"))
	if dsn == "" {
		t.Skip("set TEST_POSTGRES_DSN to run the PostgreSQL pooler-compatibility test")
	}

	db, err := openPostgres(dsn)
	require.NoError(t, err)
	sqlDB, err := db.DB()
	require.NoError(t, err)
	t.Cleanup(func() { _ = sqlDB.Close() })

	// 首次迁移走建表路径;对已存在的表再次迁移才会执行 ColumnTypes,
	// 即线上崩溃的路径。
	require.NoError(t, db.AutoMigrate(&poolerCompatWidget{}))
	require.NoError(t, db.AutoMigrate(&poolerCompatWidget{}))

	columnTypes, err := db.Migrator().ColumnTypes(&poolerCompatWidget{})
	require.NoError(t, err)
	var quotaFound bool
	for _, columnType := range columnTypes {
		if strings.EqualFold(columnType.Name(), "quota") {
			quotaFound = true
			require.Equal(t, "int8", strings.ToLower(columnType.DatabaseTypeName()))
		}
	}
	require.True(t, quotaFound)
}
