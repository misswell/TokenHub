package server

import "gorm.io/gorm"

// Match strings.TrimSpace, including tabs and Unicode whitespace, when the
// native approval resolver compares a project's quota reference.
const dopQuotaRefWhitespace = "\t\n\v\f\r \u0085\u00a0\u1680\u2000\u2001\u2002\u2003\u2004\u2005\u2006\u2007\u2008\u2009\u200a\u2028\u2029\u202f\u205f\u3000"

func dopQuotaRefExpression(db *gorm.DB) string {
	function := "TRIM"
	if db.Dialector.Name() == "postgres" {
		function = "BTRIM"
	}
	// Both the field name and trim characters are constants, never user input.
	return function + "(default_quota_ref, '" + dopQuotaRefWhitespace + "')"
}

func initializeDOPBatchIndex(db *gorm.DB) error {
	if err := db.Exec("CREATE INDEX IF NOT EXISTS idx_dop_projects_quota_ref ON projects (" + dopQuotaRefExpression(db) + ")").Error; err != nil {
		return err
	}
	return db.Exec("CREATE INDEX IF NOT EXISTS idx_dop_admin_users_created_at ON admin_users (created_at)").Error
}
