/*
 * go-mydumper
 * xelabs.org
 *
 * Copyright (c) XeLabs
 * GPL License
 *
 */

package common

import (
	"encoding/csv"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/xelabs/go-mydumper/config"
	querypb "github.com/xelabs/go-mysqlstack/sqlparser/depends/query"
	"github.com/xelabs/go-mysqlstack/xlog"
)

func writeMetaData(args *config.Config) {
	file := fmt.Sprintf("%s/metadata", args.Outdir)
	WriteFile(file, "")
}

func dumpDatabaseSchema(log *xlog.Log, conn *Connection, args *config.Config, database string) {
	schema := fmt.Sprintf("CREATE DATABASE IF NOT EXISTS `%s`;", database)
	file := fmt.Sprintf("%s/%s-schema-create.sql", args.Outdir, database)
	WriteFile(file, schema)
	log.Info("dumping.database[%s].schema...", database)
}

func dumpTableSchema(log *xlog.Log, conn *Connection, args *config.Config, database string, table string) {
	qr, err := conn.Fetch(fmt.Sprintf("SHOW CREATE TABLE `%s`.`%s`", database, table))
	AssertNil(err)
	schema := qr.Rows[0][1].String() + ";\n"

	file := fmt.Sprintf("%s/%s.%s-schema.sql", args.Outdir, database, table)
	WriteFile(file, schema)
	log.Info("dumping.table[%s.%s].schema...", database, table)
}

// Dump a table in "MySQL" (multi-inserts) format
func dumpTable(log *xlog.Log, conn *Connection, args *config.Config, database string, table string) {
	var allBytes uint64
	var allRows uint64
	var where string
	var selfields []string

	fields := make([]string, 0, 16)
	{
		cursor, err := conn.StreamFetch(fmt.Sprintf("SELECT * FROM `%s`.`%s` LIMIT 1", database, table))
		AssertNil(err)

		flds := cursor.Fields()
		for _, fld := range flds {
			log.Debug("dump -- %#v, %s, %s", args.Filters, table, fld.Name)
			if _, ok := args.Filters[table][fld.Name]; ok {
				continue
			}

			fields = append(fields, fmt.Sprintf("`%s`", fld.Name))
			replacement, ok := args.Selects[table][fld.Name]
			if ok {
				selfields = append(selfields, fmt.Sprintf("%s AS `%s`", replacement, fld.Name))
			} else {
				selfields = append(selfields, fmt.Sprintf("`%s`", fld.Name))
			}
		}
		err = cursor.Close()
		AssertNil(err)
	}

	if v, ok := args.Wheres[table]; ok {
		where = fmt.Sprintf(" WHERE %v", v)
	}

	cursor, err := conn.StreamFetch(fmt.Sprintf("SELECT %s FROM `%s`.`%s` %s", strings.Join(selfields, ", "), database, table, where))
	AssertNil(err)

	fileNo := 1
	stmtsize := 0
	chunkbytes := 0
	rows := make([]string, 0, 256)
	inserts := make([]string, 0, 256)
	for cursor.Next() {
		row, err := cursor.RowValues()
		AssertNil(err)

		values := make([]string, 0, 16)
		for _, v := range row {
			if v.Raw() == nil {
				values = append(values, "NULL")
			} else {
				str := v.String()
				switch {
				case v.IsSigned(), v.IsUnsigned(), v.IsFloat(), v.IsIntegral(), v.Type() == querypb.Type_DECIMAL:
					values = append(values, str)
				default:
					values = append(values, fmt.Sprintf("\"%s\"", EscapeBytes(v.Raw())))
				}
			}
		}
		r := "(" + strings.Join(values, ",") + ")"
		rows = append(rows, r)

		allRows++
		stmtsize += len(r)
		chunkbytes += len(r)
		allBytes += uint64(len(r))
		atomic.AddUint64(&args.Allbytes, uint64(len(r)))
		atomic.AddUint64(&args.Allrows, 1)

		if stmtsize >= args.StmtSize {
			insertone := fmt.Sprintf("INSERT INTO `%s`(%s) VALUES\n%s", table, strings.Join(fields, ","), strings.Join(rows, ",\n"))
			inserts = append(inserts, insertone)
			rows = rows[:0]
			stmtsize = 0
		}

		if (chunkbytes / 1024 / 1024) >= args.ChunksizeInMB {
			query := strings.Join(inserts, ";\n") + ";\n"
			file := fmt.Sprintf("%s/%s.%s.%05d.sql", args.Outdir, database, table, fileNo)
			WriteFile(file, query)

			log.Info("dumping.table[%s.%s].rows[%v].bytes[%vMB].part[%v].thread[%d]", database, table, allRows, (allBytes / 1024 / 1024), fileNo, conn.ID)
			inserts = inserts[:0]
			chunkbytes = 0
			fileNo++
		}
	}
	if chunkbytes > 0 {
		if len(rows) > 0 {
			insertone := fmt.Sprintf("INSERT INTO `%s`(%s) VALUES\n%s", table, strings.Join(fields, ","), strings.Join(rows, ",\n"))
			inserts = append(inserts, insertone)
		}

		query := strings.Join(inserts, ";\n") + ";\n"
		file := fmt.Sprintf("%s/%s.%s.%05d.sql", args.Outdir, database, table, fileNo)
		WriteFile(file, query)
	}
	err = cursor.Close()
	AssertNil(err)

	log.Info("dumping.table[%s.%s].done.allrows[%v].allbytes[%vMB].thread[%d]...", database, table, allRows, (allBytes / 1024 / 1024), conn.ID)
}

// Dump a table in CSV/TSV format
func dumpTableCsv(log *xlog.Log, conn *Connection, args *config.Config, database string, table string, separator rune) {
	var allBytes uint64
	var allRows uint64
	var where string
	var selfields []string
	var headerfields []string

	fields := make([]string, 0, 16)
	{
		cursor, err := conn.StreamFetch(fmt.Sprintf("SELECT * FROM `%s`.`%s` LIMIT 1", database, table))
		AssertNil(err)

		flds := cursor.Fields()
		for _, fld := range flds {
			log.Debug("dump -- %#v, %s, %s", args.Filters, table, fld.Name)
			if _, ok := args.Filters[table][fld.Name]; ok {
				continue
			}

			fields = append(fields, fmt.Sprintf("`%s`", fld.Name))
			headerfields = append(headerfields, fld.Name)
			replacement, ok := args.Selects[table][fld.Name]
			if ok {
				selfields = append(selfields, fmt.Sprintf("%s AS `%s`", replacement, fld.Name))
			} else {
				selfields = append(selfields, fmt.Sprintf("`%s`", fld.Name))
			}
		}
		err = cursor.Close()
		AssertNil(err)
	}

	if v, ok := args.Wheres[table]; ok {
		where = fmt.Sprintf(" WHERE %v", v)
	}

	cursor, err := conn.StreamFetch(fmt.Sprintf("SELECT %s FROM `%s`.`%s` %s", strings.Join(selfields, ", "), database, table, where))
	AssertNil(err)

	fileNo := 1
	file, err := os.Create(fmt.Sprintf("%s/%s.%s.%05d.csv", args.Outdir, database, table, fileNo))
	AssertNil(err)
	writer := csv.NewWriter(file)
	writer.Comma = separator
	writer.Write(headerfields)

	chunkbytes := 0
	rows := make([]string, 0, 256)
	rows = append(rows, strings.Join(headerfields, "\t"))
	inserts := make([]string, 0, 256)
	for cursor.Next() {
		row, err := cursor.RowValues()
		AssertNil(err)

		values := make([]string, 0, 16)
		rowsize := 0
		for _, v := range row {
			if v.Raw() == nil {
				values = append(values, "NULL")
				rowsize += 4
			} else {
				str := v.String()
				switch {
				case v.IsSigned(), v.IsUnsigned(), v.IsFloat(), v.IsIntegral(), v.Type() == querypb.Type_DECIMAL:
					values = append(values, str)
					rowsize += len(str)
				default:
					values = append(values, fmt.Sprintf("%s", EscapeBytes(v.Raw())))
					rowsize += len(v.Raw())
				}
			}
		}
		writer.Write(values)
		chunkbytes += rowsize

		allRows++
		atomic.AddUint64(&args.Allbytes, uint64(rowsize))
		atomic.AddUint64(&args.Allrows, 1)

		if (chunkbytes / 1024 / 1024) >= args.ChunksizeInMB {
			writer.Flush()
			file, err := os.Create(fmt.Sprintf("%s/%s.%s.%05d.csv", args.Outdir, database, table, fileNo))
			AssertNil(err)
			writer = csv.NewWriter(file)
			writer.Comma = separator
			writer.Write(headerfields)
			log.Info("dumping.table[%s.%s].rows[%v].bytes[%vMB].part[%v].thread[%d]", database, table, allRows, (allBytes / 1024 / 1024), fileNo, conn.ID)
			inserts = inserts[:0]
			chunkbytes = 0
			fileNo++
		}
	}
	writer.Flush()
	err = cursor.Close()
	AssertNil(err)

	log.Info("dumping.table[%s.%s].done.allrows[%v].allbytes[%vMB].thread[%d]...", database, table, allRows, (allBytes / 1024 / 1024), conn.ID)
}

func allTables(log *xlog.Log, conn *Connection, database string) []string {
	qr, err := conn.Fetch(fmt.Sprintf("SHOW TABLES FROM `%s`", database))
	AssertNil(err)

	tables := make([]string, 0, 128)
	for _, t := range qr.Rows {
		tables = append(tables, t[0].String())
	}
	return tables
}

func allDatabases(log *xlog.Log, conn *Connection) []string {
	qr, err := conn.Fetch("SHOW DATABASES")
	AssertNil(err)

	databases := make([]string, 0, 128)
	for _, t := range qr.Rows {
		databases = append(databases, t[0].String())
	}
	return databases
}

func filterDatabases(log *xlog.Log, conn *Connection, filter *regexp.Regexp, invert bool) []string {
	qr, err := conn.Fetch("SHOW DATABASES")
	AssertNil(err)

	databases := make([]string, 0, 128)
	for _, t := range qr.Rows {
		if (!invert && filter.MatchString(t[0].String())) || (invert && !filter.MatchString(t[0].String())) {
			databases = append(databases, t[0].String())
		}
	}
	return databases
}

// Dumper used to start the dumper worker.
func Dumper(log *xlog.Log, args *config.Config) {
	initPool, err := NewPool(log, args.Threads, args.Address, args.User, args.Password, "", "")
	AssertNil(err)
	defer initPool.Close()

	// Meta data.
	writeMetaData(args)

	// database.
	conn := initPool.Get()
	var databases []string
	t := time.Now()
	if args.DatabaseRegexp != "" {
		r := regexp.MustCompile(args.DatabaseRegexp)
		databases = filterDatabases(log, conn, r, args.DatabaseInvertRegexp)
	} else {
		if args.Database != "" {
			databases = strings.Split(args.Database, ",")
		} else {
			databases = allDatabases(log, conn)
		}
	}
	for _, database := range databases {
		dumpDatabaseSchema(log, conn, args, database)
	}

	tables := make([][]string, len(databases))
	for i, database := range databases {
		if args.Table != "" {
			tables[i] = strings.Split(args.Table, ",")
		} else {
			tables[i] = allTables(log, conn, database)
		}
	}
	initPool.Put(conn)

	var wg sync.WaitGroup
	for i, database := range databases {
		pool, err := NewPool(log, args.Threads/len(databases), args.Address, args.User, args.Password, args.SessionVars, database)
		AssertNil(err)
		defer pool.Close()
		for _, table := range tables[i] {
			conn := initPool.Get()
			dumpTableSchema(log, conn, args, database, table)
			initPool.Put(conn)

			conn = pool.Get()
			wg.Add(1)
			go func(conn *Connection, database string, table string) {
				defer func() {
					wg.Done()
					pool.Put(conn)
				}()
				log.Info("dumping.table[%s.%s].datas.thread[%d]...", database, table, conn.ID)
				if args.Format == "mysql" {
					dumpTable(log, conn, args, database, table)
				} else if args.Format == "tsv" {
					dumpTableCsv(log, conn, args, database, table, '\t')
				} else if args.Format == "csv" {
					dumpTableCsv(log, conn, args, database, table, ',')
				} else {
					AssertNil(errors.New("Unknown dump format"))
				}

				log.Info("dumping.table[%s.%s].datas.thread[%d].done...", database, table, conn.ID)
			}(conn, database, table)
		}
	}

	tick := time.NewTicker(time.Millisecond * time.Duration(args.IntervalMs))
	defer tick.Stop()
	go func() {
		for range tick.C {
			diff := time.Since(t).Seconds()
			allbytesMB := float64(atomic.LoadUint64(&args.Allbytes) / 1024 / 1024)
			allrows := atomic.LoadUint64(&args.Allrows)
			rates := allbytesMB / diff
			log.Info("dumping.allbytes[%vMB].allrows[%v].time[%.2fsec].rates[%.2fMB/sec]...", allbytesMB, allrows, diff, rates)
		}
	}()

	wg.Wait()
	elapsed := time.Since(t).Seconds()
	log.Info("dumping.all.done.cost[%.2fsec].allrows[%v].allbytes[%v].rate[%.2fMB/s]", elapsed, args.Allrows, args.Allbytes, (float64(args.Allbytes/1024/1024) / elapsed))
}
