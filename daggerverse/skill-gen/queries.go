package main

// Introspection queries, ported from the original postgres-skill-creator's
// scripts/introspect.sh. Each returns a JSON array (via the postgres module's
// Client.QueryJSON) keyed by the aliased column names that the ./skill Model's
// Parse* functions expect. Reserved-word aliases (schema, table, column,
// index, type, view, comment) are double-quoted so the JSON key stays exactly
// that lowercase word. Every query restricts to user schemas
// (NOT IN pg_catalog/information_schema).
const (
	// tablesSQL: one row per column of every base table, in ordinal order.
	// USER-DEFINED types resolve to their underlying udt_name (e.g. an enum).
	tablesSQL = `
		SELECT c.table_schema AS "schema",
		       c.table_name   AS "table",
		       c.column_name  AS "column",
		       CASE WHEN c.data_type = 'USER-DEFINED' THEN c.udt_name ELSE c.data_type END AS "type",
		       c.is_nullable  AS nullable,
		       COALESCE(c.column_default, '') AS default_value
		FROM information_schema.columns c
		JOIN information_schema.tables t
		  ON t.table_schema = c.table_schema AND t.table_name = c.table_name
		WHERE c.table_schema NOT IN ('pg_catalog', 'information_schema')
		  AND t.table_type = 'BASE TABLE'
		ORDER BY c.table_schema, c.table_name, c.ordinal_position;`

	// primaryKeysSQL: one row per primary-key column, in key order.
	primaryKeysSQL = `
		SELECT tc.table_schema AS "schema",
		       tc.table_name   AS "table",
		       kcu.column_name AS "column"
		FROM information_schema.table_constraints tc
		JOIN information_schema.key_column_usage kcu
		  ON tc.constraint_schema = kcu.constraint_schema
		 AND tc.constraint_name   = kcu.constraint_name
		WHERE tc.constraint_type = 'PRIMARY KEY'
		  AND tc.table_schema NOT IN ('pg_catalog', 'information_schema')
		ORDER BY tc.table_schema, tc.table_name, kcu.ordinal_position;`

	// foreignKeysSQL: pg_constraint with unnest WITH ORDINALITY so composite
	// FKs pair referencing/referenced columns correctly (the information_schema
	// variant cross-products them).
	foreignKeysSQL = `
		SELECT src_ns.nspname  AS "schema",
		       src_tbl.relname AS "table",
		       src_col.attname AS "column",
		       ref_ns.nspname  AS ref_schema,
		       ref_tbl.relname AS ref_table,
		       ref_col.attname AS ref_column
		FROM pg_constraint con
		JOIN pg_class src_tbl       ON src_tbl.oid = con.conrelid
		JOIN pg_namespace src_ns    ON src_ns.oid = src_tbl.relnamespace
		JOIN pg_class ref_tbl       ON ref_tbl.oid = con.confrelid
		JOIN pg_namespace ref_ns    ON ref_ns.oid = ref_tbl.relnamespace
		JOIN LATERAL unnest(con.conkey)  WITH ORDINALITY AS src_key(attnum, ord) ON TRUE
		JOIN LATERAL unnest(con.confkey) WITH ORDINALITY AS ref_key(attnum, ord) ON ref_key.ord = src_key.ord
		JOIN pg_attribute src_col   ON src_col.attrelid = src_tbl.oid AND src_col.attnum = src_key.attnum
		JOIN pg_attribute ref_col   ON ref_col.attrelid = ref_tbl.oid AND ref_col.attnum = ref_key.attnum
		WHERE con.contype = 'f'
		  AND src_ns.nspname NOT IN ('pg_catalog', 'information_schema')
		ORDER BY src_ns.nspname, src_tbl.relname, src_key.ord;`

	// indexesSQL: one row per index with its full CREATE INDEX definition.
	indexesSQL = `
		SELECT schemaname AS "schema",
		       tablename  AS "table",
		       indexname  AS "index",
		       indexdef   AS definition
		FROM pg_indexes
		WHERE schemaname NOT IN ('pg_catalog', 'information_schema')
		ORDER BY schemaname, tablename, indexname;`

	// enumsSQL: one row per enum label, in enumsortorder.
	enumsSQL = `
		SELECT n.nspname   AS "schema",
		       t.typname   AS "type",
		       e.enumlabel AS label
		FROM pg_type t
		JOIN pg_enum e      ON t.oid = e.enumtypid
		JOIN pg_namespace n ON n.oid = t.typnamespace
		WHERE n.nspname NOT IN ('pg_catalog', 'information_schema')
		ORDER BY n.nspname, t.typname, e.enumsortorder;`

	// viewsSQL: one row per view with its definition inline. JSON tolerates the
	// multi-line SQL, so (unlike the TSV-based original) no separate per-view
	// file is needed.
	viewsSQL = `
		SELECT n.nspname AS "schema",
		       c.relname AS "view",
		       pg_get_viewdef(c.oid, true) AS definition
		FROM pg_class c
		JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE c.relkind = 'v'
		  AND n.nspname NOT IN ('pg_catalog', 'information_schema')
		ORDER BY n.nspname, c.relname;`

	// commentsSQL: table-level (column = '') and column-level COMMENTs, with
	// tabs/newlines flattened to spaces so each stays a single value.
	commentsSQL = `
		SELECT n.nspname AS "schema",
		       c.relname AS relation,
		       COALESCE(a.attname, '') AS "column",
		       regexp_replace(d.description, '[\t\n\r]', ' ', 'g') AS "comment"
		FROM pg_description d
		JOIN pg_class c     ON d.objoid = c.oid
		JOIN pg_namespace n ON c.relnamespace = n.oid
		LEFT JOIN pg_attribute a ON a.attrelid = c.oid AND a.attnum = d.objsubid
		WHERE n.nspname NOT IN ('pg_catalog', 'information_schema')
		ORDER BY n.nspname, c.relname;`
)
