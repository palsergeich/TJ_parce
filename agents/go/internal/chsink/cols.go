// cols.go — колоночные буферы native-блока ClickHouse (ch-go/proto).
//
// Замена построчного batch.Append clickhouse-go: строки копятся в типизированные
// колонки (append в слайсы + memcpy строк в общий буфер ColStr), блок уходит
// одним INSERT (client.Do с Input). Кодирование LowCardinality (словарь+индексы)
// делает Prepare() ch-go на этапе отправки блока.
package chsink

import (
	"time"

	"github.com/ClickHouse/ch-go/proto"
)

// colSet — колоночный буфер одной из двух схем.
type colSet interface {
	appendRow(r *Row)
	rows() int
	input() proto.Input
	reset()
}

// ---------------------------------------------------------------- bench ---

// benchCols — таблица bench-схемы (bench/clickhouse/bench_schema.sql):
// timestamp DateTime64(6), duration UInt64, event/level LowCardinality(String),
// filename/file_path String, props Map(LowCardinality(String), String).
type benchCols struct {
	ts       *proto.ColDateTime64
	duration proto.ColUInt64
	event    *proto.ColLowCardinality[string]
	level    *proto.ColLowCardinality[string]
	filename proto.ColStr
	filePath proto.ColStr

	propsKeys *proto.ColLowCardinality[string]
	propsVals proto.ColStr
	propsOffs proto.ColUInt64
}

func newBenchCols() *benchCols {
	return &benchCols{
		ts:        (&proto.ColDateTime64{}).WithPrecision(proto.PrecisionMicro),
		event:     proto.NewLowCardinality(new(proto.ColStr)),
		level:     proto.NewLowCardinality(new(proto.ColStr)),
		propsKeys: proto.NewLowCardinality(new(proto.ColStr)),
	}
}

func (c *benchCols) appendRow(r *Row) {
	c.ts.Append(r.Time)
	c.duration.Append(r.Duration)
	c.event.Append(r.Event)
	c.level.Append(r.Level)
	c.filename.Append(r.Filename)
	c.filePath.Append(r.FilePath)
	for i := range r.Props {
		c.propsKeys.Append(r.Props[i].Name)
		c.propsVals.Append(r.Props[i].Value)
	}
	c.propsOffs.Append(uint64(c.propsKeys.Rows()))
}

func (c *benchCols) rows() int { return c.ts.Rows() }

func (c *benchCols) input() proto.Input {
	return proto.Input{
		{Name: "timestamp", Data: c.ts},
		{Name: "duration", Data: &c.duration},
		{Name: "event", Data: c.event},
		{Name: "level", Data: c.level},
		{Name: "filename", Data: &c.filename},
		{Name: "file_path", Data: &c.filePath},
		{Name: "props", Data: &proto.ColMap[string, string]{
			Offsets: c.propsOffs,
			Keys:    c.propsKeys,
			Values:  &c.propsVals,
		}},
	}
}

func (c *benchCols) reset() {
	c.ts.Data = c.ts.Data[:0]
	c.duration = c.duration[:0]
	c.event.Reset()
	c.level.Reset()
	c.filename.Reset()
	c.filePath.Reset()
	c.propsKeys.Reset()
	c.propsVals.Reset()
	c.propsOffs = c.propsOffs[:0]
}

// benchInsertColumns — список колонок INSERT (порядок = input()).
const benchInsertColumns = "(timestamp, duration, event, level, filename, file_path, props)"

// ----------------------------------------------------------------- rich ---

// richCols — продуктовая таблица tj.events (deploy/clickhouse/init/001_schema.sql),
// 47 колонок; порядок INSERT повторяет импортёр import-jsonl.ps1.
type richCols struct {
	ts         *proto.ColDateTime64
	durationUs proto.ColUInt64
	event      *proto.ColLowCardinality[string]
	level      *proto.ColLowCardinality[string]
	collection *proto.ColLowCardinality[string]
	srcFile    *proto.ColLowCardinality[string]
	srcPath    proto.ColStr
	srcLine    proto.ColUInt32

	process      *proto.ColLowCardinality[string]
	processName  *proto.ColLowCardinality[string]
	osThread     proto.ColUInt32
	clientID     proto.ColUInt32
	connectID    proto.ColUInt32
	sessionID    proto.ColUInt32
	usr          *proto.ColLowCardinality[string]
	appName      *proto.ColLowCardinality[string]
	computerName *proto.ColLowCardinality[string]
	appID        *proto.ColLowCardinality[string]

	dbms         *proto.ColLowCardinality[string]
	dbName       *proto.ColLowCardinality[string]
	dbPid        proto.ColUInt32
	trans        proto.ColUInt8
	rowsRet      proto.ColUInt64
	rowsAffected proto.ColUInt64

	cpuTimeUs  proto.ColUInt64
	memory     proto.ColInt64
	memoryPeak proto.ColInt64
	inBytes    proto.ColUInt64
	outBytes   proto.ColUInt64
	callWaitUs proto.ColUInt64

	ifaceName  *proto.ColLowCardinality[string]
	methodName *proto.ColLowCardinality[string]
	funcName   *proto.ColLowCardinality[string]
	module     *proto.ColLowCardinality[string]

	context     proto.ColStr
	contextHash proto.ColUInt64
	contextLine *proto.ColLowCardinality[string]
	sqlText     proto.ColStr
	sqlHash     proto.ColUInt64
	planText    proto.ColStr
	descr       proto.ColStr
	exception   *proto.ColLowCardinality[string]

	lockRegions   proto.ColStr
	lockWaitData  proto.ColUInt32 // элементы Array(UInt32)
	lockWaitOffs  proto.ColUInt64
	locksDump     proto.ColStr
	deadlockGraph proto.ColStr

	propsKeys *proto.ColLowCardinality[string]
	propsVals proto.ColStr
	propsOffs proto.ColUInt64
}

func newRichCols() *richCols {
	lc := func() *proto.ColLowCardinality[string] {
		return proto.NewLowCardinality(new(proto.ColStr))
	}
	return &richCols{
		ts:           (&proto.ColDateTime64{}).WithPrecision(proto.PrecisionMicro).WithLocation(time.UTC),
		event:        lc(),
		level:        lc(),
		collection:   lc(),
		srcFile:      lc(),
		process:      lc(),
		processName:  lc(),
		usr:          lc(),
		appName:      lc(),
		computerName: lc(),
		appID:        lc(),
		dbms:         lc(),
		dbName:       lc(),
		ifaceName:    lc(),
		methodName:   lc(),
		funcName:     lc(),
		module:       lc(),
		contextLine:  lc(),
		exception:    lc(),
		propsKeys:    lc(),
	}
}

func (c *richCols) appendRow(r *Row) {
	x := r.Rich
	c.ts.Append(x.Time)
	c.durationUs.Append(x.DurationUs)
	c.event.Append(r.Event)
	c.level.Append(r.Level)
	c.collection.Append(x.Collection)
	c.srcFile.Append(r.Filename)
	c.srcPath.Append(r.FilePath)
	c.srcLine.Append(0) // нормализатор не отдаёт номер строки (v1.1: src_offset)

	c.process.Append(x.Process)
	c.processName.Append(x.ProcessName)
	c.osThread.Append(x.OSThread)
	c.clientID.Append(x.ClientID)
	c.connectID.Append(x.ConnectID)
	c.sessionID.Append(x.SessionID)
	c.usr.Append(x.Usr)
	c.appName.Append(x.AppName)
	c.computerName.Append(x.ComputerName)
	c.appID.Append(x.AppID)

	c.dbms.Append(x.DBMS)
	c.dbName.Append(x.DBName)
	c.dbPid.Append(x.DBPid)
	c.trans.Append(x.Trans)
	c.rowsRet.Append(x.RowsRet)
	c.rowsAffected.Append(x.RowsAffected)

	c.cpuTimeUs.Append(x.CPUTimeUs)
	c.memory.Append(x.Memory)
	c.memoryPeak.Append(x.MemoryPeak)
	c.inBytes.Append(x.InBytes)
	c.outBytes.Append(x.OutBytes)
	c.callWaitUs.Append(x.CallWaitUs)

	c.ifaceName.Append(x.IfaceName)
	c.methodName.Append(x.MethodName)
	c.funcName.Append(x.FuncName)
	c.module.Append(x.Module)

	c.context.Append(x.Context)
	c.contextHash.Append(x.ContextHash)
	c.contextLine.Append(x.ContextLine)
	c.sqlText.Append(x.SQLText)
	c.sqlHash.Append(x.SQLHash)
	c.planText.Append(x.PlanText)
	c.descr.Append(x.Descr)
	c.exception.Append(x.Exception)

	c.lockRegions.Append(x.LockRegions)
	for _, v := range x.LockWaitConns {
		c.lockWaitData.Append(v)
	}
	c.lockWaitOffs.Append(uint64(len(c.lockWaitData)))
	c.locksDump.Append(x.LocksDump)
	c.deadlockGraph.Append(x.DeadlockGraph)

	for i := range r.Props {
		c.propsKeys.Append(r.Props[i].Name)
		c.propsVals.Append(r.Props[i].Value)
	}
	c.propsOffs.Append(uint64(c.propsKeys.Rows()))
}

func (c *richCols) rows() int { return c.ts.Rows() }

func (c *richCols) input() proto.Input {
	return proto.Input{
		{Name: "ts", Data: c.ts},
		{Name: "duration_us", Data: &c.durationUs},
		{Name: "event", Data: c.event},
		{Name: "level", Data: c.level},
		{Name: "collection", Data: c.collection},
		{Name: "src_file", Data: c.srcFile},
		{Name: "src_path", Data: &c.srcPath},
		{Name: "src_line", Data: &c.srcLine},
		{Name: "process", Data: c.process},
		{Name: "process_name", Data: c.processName},
		{Name: "os_thread", Data: &c.osThread},
		{Name: "client_id", Data: &c.clientID},
		{Name: "connect_id", Data: &c.connectID},
		{Name: "session_id", Data: &c.sessionID},
		{Name: "usr", Data: c.usr},
		{Name: "app_name", Data: c.appName},
		{Name: "computer_name", Data: c.computerName},
		{Name: "app_id", Data: c.appID},
		{Name: "dbms", Data: c.dbms},
		{Name: "db_name", Data: c.dbName},
		{Name: "db_pid", Data: &c.dbPid},
		{Name: "trans", Data: &c.trans},
		{Name: "rows_ret", Data: &c.rowsRet},
		{Name: "rows_affected", Data: &c.rowsAffected},
		{Name: "cpu_time_us", Data: &c.cpuTimeUs},
		{Name: "memory", Data: &c.memory},
		{Name: "memory_peak", Data: &c.memoryPeak},
		{Name: "in_bytes", Data: &c.inBytes},
		{Name: "out_bytes", Data: &c.outBytes},
		{Name: "call_wait_us", Data: &c.callWaitUs},
		{Name: "iface_name", Data: c.ifaceName},
		{Name: "method_name", Data: c.methodName},
		{Name: "func_name", Data: c.funcName},
		{Name: "module", Data: c.module},
		{Name: "context", Data: &c.context},
		{Name: "context_hash", Data: &c.contextHash},
		{Name: "context_line", Data: c.contextLine},
		{Name: "sql_text", Data: &c.sqlText},
		{Name: "sql_hash", Data: &c.sqlHash},
		{Name: "plan_text", Data: &c.planText},
		{Name: "descr", Data: &c.descr},
		{Name: "exception", Data: c.exception},
		{Name: "lock_regions", Data: &c.lockRegions},
		{Name: "lock_wait_conns", Data: &proto.ColArr[uint32]{
			Offsets: c.lockWaitOffs,
			Data:    &c.lockWaitData,
		}},
		{Name: "locks_dump", Data: &c.locksDump},
		{Name: "deadlock_graph", Data: &c.deadlockGraph},
		{Name: "props", Data: &proto.ColMap[string, string]{
			Offsets: c.propsOffs,
			Keys:    c.propsKeys,
			Values:  &c.propsVals,
		}},
	}
}

func (c *richCols) reset() {
	c.ts.Data = c.ts.Data[:0]
	c.durationUs = c.durationUs[:0]
	c.event.Reset()
	c.level.Reset()
	c.collection.Reset()
	c.srcFile.Reset()
	c.srcPath.Reset()
	c.srcLine = c.srcLine[:0]
	c.process.Reset()
	c.processName.Reset()
	c.osThread = c.osThread[:0]
	c.clientID = c.clientID[:0]
	c.connectID = c.connectID[:0]
	c.sessionID = c.sessionID[:0]
	c.usr.Reset()
	c.appName.Reset()
	c.computerName.Reset()
	c.appID.Reset()
	c.dbms.Reset()
	c.dbName.Reset()
	c.dbPid = c.dbPid[:0]
	c.trans = c.trans[:0]
	c.rowsRet = c.rowsRet[:0]
	c.rowsAffected = c.rowsAffected[:0]
	c.cpuTimeUs = c.cpuTimeUs[:0]
	c.memory = c.memory[:0]
	c.memoryPeak = c.memoryPeak[:0]
	c.inBytes = c.inBytes[:0]
	c.outBytes = c.outBytes[:0]
	c.callWaitUs = c.callWaitUs[:0]
	c.ifaceName.Reset()
	c.methodName.Reset()
	c.funcName.Reset()
	c.module.Reset()
	c.context.Reset()
	c.contextHash = c.contextHash[:0]
	c.contextLine.Reset()
	c.sqlText.Reset()
	c.sqlHash = c.sqlHash[:0]
	c.planText.Reset()
	c.descr.Reset()
	c.exception.Reset()
	c.lockRegions.Reset()
	c.lockWaitData = c.lockWaitData[:0]
	c.lockWaitOffs = c.lockWaitOffs[:0]
	c.locksDump.Reset()
	c.deadlockGraph.Reset()
	c.propsKeys.Reset()
	c.propsVals.Reset()
	c.propsOffs = c.propsOffs[:0]
}

// richInsertColumns — список колонок INSERT (порядок = input()).
const richInsertColumns = "(ts, duration_us, event, level, collection, src_file, src_path, src_line, " +
	"process, process_name, os_thread, client_id, connect_id, session_id, usr, " +
	"app_name, computer_name, app_id, " +
	"dbms, db_name, db_pid, trans, rows_ret, rows_affected, " +
	"cpu_time_us, memory, memory_peak, in_bytes, out_bytes, call_wait_us, " +
	"iface_name, method_name, func_name, module, " +
	"context, context_hash, context_line, sql_text, sql_hash, plan_text, descr, exception, " +
	"lock_regions, lock_wait_conns, locks_dump, deadlock_graph, props)"
