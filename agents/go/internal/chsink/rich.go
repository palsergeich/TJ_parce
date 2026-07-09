// rich.go — маппинг события ТЖ в продуктовую схему tj.events
// (deploy/clickhouse/init/001_schema.sql). Эталон семантики — импортёр
// deploy/importer/import-jsonl.ps1: он читает NDJSON агента и раскладывает
// свойства по горячим колонкам выражениями ClickHouse. Здесь те же правила
// применяются к сырым разобранным полям события; эквивалентность закреплена
// parity-приёмкой (см. README §Продуктовая схема) и юнит-тестами.
//
// Ключевые правила импортёра, воспроизводимые дословно:
//   - m[k] по Map с дубликатами ключей возвращает ПЕРВОЕ значение → горячие
//     колонки берут первое вхождение свойства (NDJSON сохраняет дубликаты);
//   - хвост props сохраняет ВСЕ невыбранные пары, включая дубликаты ключей;
//   - имена NDJSON-мета (timestamp/duration/event/level/filename/file_path)
//     исключаются из props безусловно — даже если это свойство самого события;
//   - SessionID: session_id = toUInt32OrZero(первого значения); каждая пара
//     SessionID остаётся в props, только если toUInt32OrZero(v)=0 и v∉{”,'0'};
//   - client_id: t:clientID, при пустом/отсутствующем — ClientID (оба всегда
//     исключаются из props);
//   - sql_text: первый непустой из Sql|Query|Sdbl; descr: Descr|Txt|txt;
//   - context_hash/sql_hash: cityHash64 сырого текста, 0 при пустом
//     (github.com/go-faster/city == cityHash64 ClickHouse, проверено батареей
//     на живом сервере — 25/25 строк, включая UTF-8 и \r\n);
//   - context_line: последняя непустая строка Context после удаления всех \r;
//     при включённом context_skd_smart для хвостов СКД берётся строка
//     модуля-«виновника» выше по стеку (ctxline.go, docs/context-line.md);
//   - ts: только валидная дата (parseDateTime64BestEffortOrNull отвергает
//     месяц 13 / 30 февраля / минуту 60 — в отличие от нормализации
//     time.Date в bench-пути), иначе эпоха 1970-01-01;
//   - duration_us: toUInt64OrZero — переполнение uint64 даёт 0 (bench-путь
//     насыщает до MaxUint64; расхождение возможно только при длительности
//     > 1.8e19 мкс, в реальных журналах не встречается).
package chsink

import (
	"math"
	"time"

	"github.com/go-faster/city"

	"tjagent/internal/metrics"
	"tjagent/internal/sqlnorm"
)

// RichExt — вычисленные поля продуктовой схемы (всё, чего нет в базовой
// части Row). Заполняется RowBuilder'ом в режиме rich; props-хвост события
// лежит в Row.Props (без дедупликации, в порядке события).
type RichExt struct {
	Time       time.Time // ts с валидацией диапазонов (иначе эпоха)
	DurationUs uint64    // toUInt64OrZero от сырого токена длительности

	Collection  string
	Process     string
	ProcessName string

	OSThread  uint32
	ClientID  uint32
	ConnectID uint32
	SessionID uint32

	Usr          string
	AppName      string
	ComputerName string
	AppID        string

	DBMS   string
	DBName string
	DBPid  uint32
	Trans  uint8

	RowsRet      uint64
	RowsAffected uint64

	CPUTimeUs  uint64
	Memory     int64
	MemoryPeak int64
	InBytes    uint64
	OutBytes   uint64
	CallWaitUs uint64

	IfaceName  string
	MethodName string
	FuncName   string
	Module     string

	Context     string
	ContextHash uint64
	ContextLine string
	SQLText     string
	SQLHash     uint64
	PlanText    string
	Descr       string
	Exception   string

	// Нормализация SQL (docs/sql-normalization.md; правила v1, диалект MSSQL).
	// Заполняется при включённом sql_norm и непустом SQLText: хэш нормы
	// (литералы → '?', хвост p_N вырезан), позиционный массив значений и его
	// длина с насыщением UInt16 (реальная длина всегда в len(SQLParams)).
	SQLNormHash uint64
	ParamCount  uint16
	SQLParams   []string

	LockRegions   string
	LockWaitConns []uint32
	LocksDump     string
	DeadlockGraph string
}

// richHot — сырьё горячих свойств: первое вхождение каждого ключа
// (семантика m[k] импортёра). Скретч RowBuilder'а, обнуляется на каждое событие.
type richHot struct {
	seen uint64 // битовая маска hotXxx — ключ уже встречался

	process, processName                    string
	osThread, tClientID, clientID, connect  string
	sessionID                               string
	usr, appName, computerName, appID       string
	dbms, dataBase, dbpid, trans            string
	rows, rowsAffected                      string
	cpuTime, memory, memoryPeak             string
	inBytes, outBytes, callWait             string
	iName, mName, funcName, module          string
	context, sql, query, sdbl, planSQLText  string
	descr, txtU, txtL, exception            string
	regions, waitConnections, locks, deadlk string
}

// Биты richHot.seen. Порядок произвольный, важна только уникальность.
const (
	hotProcess = 1 << iota
	hotProcessName
	hotOSThread
	hotTClientID
	hotClientID
	hotConnectID
	hotSessionID
	hotUsr
	hotAppName
	hotComputerName
	hotAppID
	hotDBMS
	hotDataBase
	hotDBPid
	hotTrans
	hotRows
	hotRowsAffected
	hotCPUTime
	hotMemory
	hotMemoryPeak
	hotInBytes
	hotOutBytes
	hotCallWait
	hotIName
	hotMName
	hotFunc
	hotModule
	hotContext
	hotSQL
	hotQuery
	hotSdbl
	hotPlanSQLText
	hotDescr
	hotTxtU
	hotTxtL
	hotException
	hotRegions
	hotWaitConnections
	hotLocks
	hotDeadlock
)

// set запоминает первое вхождение: повторные игнорируются (m[k] = первое).
func (h *richHot) set(bit uint64, dst *string, value []byte) {
	if h.seen&bit != 0 {
		return
	}
	h.seen |= bit
	*dst = string(value)
}

// dispatchHot пытается принять свойство name как горячее. Возвращает
// (isHot, keepInProps): isHot — ключ входит в список исключений импортёра
// (в props по умолчанию не попадает); keepInProps — пару всё же надо
// сохранить в props (условие SessionID).
func (h *richHot) dispatchHot(name, value []byte) (isHot, keepInProps bool) {
	switch string(name) { // switch string([]byte) не аллоцирует
	// NDJSON-мета: исключаются из props, колонок не задают (заголовок первичен)
	case "timestamp", "duration", "event", "level", "filename", "file_path":
		return true, false
	case "process":
		h.set(hotProcess, &h.process, value)
	case "p:processName":
		h.set(hotProcessName, &h.processName, value)
	case "OSThread":
		h.set(hotOSThread, &h.osThread, value)
	case "t:clientID":
		h.set(hotTClientID, &h.tClientID, value)
	case "ClientID":
		h.set(hotClientID, &h.clientID, value)
	case "t:connectID":
		h.set(hotConnectID, &h.connect, value)
	case "SessionID":
		h.set(hotSessionID, &h.sessionID, value)
		// Пара остаётся в props, только если значение не разобралось в число
		// и не пусто/не '0' (правило mapFilter импортёра, применяется К КАЖДОЙ паре)
		v := string(value)
		return true, chUint32OrZero(v) == 0 && v != "" && v != "0"
	case "Usr":
		h.set(hotUsr, &h.usr, value)
	case "t:applicationName":
		h.set(hotAppName, &h.appName, value)
	case "t:computerName":
		h.set(hotComputerName, &h.computerName, value)
	case "AppID":
		h.set(hotAppID, &h.appID, value)
	case "DBMS":
		h.set(hotDBMS, &h.dbms, value)
	case "DataBase":
		h.set(hotDataBase, &h.dataBase, value)
	case "dbpid":
		h.set(hotDBPid, &h.dbpid, value)
	case "Trans":
		h.set(hotTrans, &h.trans, value)
	case "Rows":
		h.set(hotRows, &h.rows, value)
	case "RowsAffected":
		h.set(hotRowsAffected, &h.rowsAffected, value)
	case "CpuTime":
		h.set(hotCPUTime, &h.cpuTime, value)
	case "Memory":
		h.set(hotMemory, &h.memory, value)
	case "MemoryPeak":
		h.set(hotMemoryPeak, &h.memoryPeak, value)
	case "InBytes":
		h.set(hotInBytes, &h.inBytes, value)
	case "OutBytes":
		h.set(hotOutBytes, &h.outBytes, value)
	case "callWait":
		h.set(hotCallWait, &h.callWait, value)
	case "IName":
		h.set(hotIName, &h.iName, value)
	case "MName":
		h.set(hotMName, &h.mName, value)
	case "Func":
		h.set(hotFunc, &h.funcName, value)
	case "Module":
		h.set(hotModule, &h.module, value)
	case "Context":
		h.set(hotContext, &h.context, value)
	case "Sql":
		h.set(hotSQL, &h.sql, value)
	case "Query":
		h.set(hotQuery, &h.query, value)
	case "Sdbl":
		h.set(hotSdbl, &h.sdbl, value)
	case "planSQLText":
		h.set(hotPlanSQLText, &h.planSQLText, value)
	case "Descr":
		h.set(hotDescr, &h.descr, value)
	case "Txt":
		h.set(hotTxtU, &h.txtU, value)
	case "txt":
		h.set(hotTxtL, &h.txtL, value)
	case "Exception":
		h.set(hotException, &h.exception, value)
	case "Regions":
		h.set(hotRegions, &h.regions, value)
	case "WaitConnections":
		h.set(hotWaitConnections, &h.waitConnections, value)
	case "Locks":
		h.set(hotLocks, &h.locks, value)
	case "DeadlockConnectionIntersections":
		h.set(hotDeadlock, &h.deadlk, value)
	default:
		return false, false
	}
	return true, false
}

// finalize превращает сырые значения в RichExt (числа, фолбэки, хэши).
// norm != nil включает нормализацию SQL (скретч воркера, см. RowBuilder);
// ctxSmart включает правило СКД для context_line (ctxline.go).
func (h *richHot) finalize(ext *RichExt, datePrefix string, timePart []byte, durationTok []byte, filePath string, norm *sqlnorm.Normalizer, ctxSmart bool) {
	ext.Time = richEventTime(datePrefix, timePart)
	ext.DurationUs = chUint64OrZero(string(durationTok))
	ext.Collection = firstPathSegment(filePath)

	ext.Process = h.process
	ext.ProcessName = h.processName
	ext.OSThread = chUint32OrZero(h.osThread)
	// client_id: t:clientID, при пустом (в т.ч. отсутствующем) — ClientID
	if h.tClientID != "" {
		ext.ClientID = chUint32OrZero(h.tClientID)
	} else {
		ext.ClientID = chUint32OrZero(h.clientID)
	}
	ext.ConnectID = chUint32OrZero(h.connect)
	ext.SessionID = chUint32OrZero(h.sessionID)
	ext.Usr = h.usr
	ext.AppName = h.appName
	ext.ComputerName = h.computerName
	ext.AppID = h.appID
	ext.DBMS = h.dbms
	ext.DBName = h.dataBase
	ext.DBPid = chUint32OrZero(h.dbpid)
	ext.Trans = chUint8OrZero(h.trans)
	ext.RowsRet = chUint64OrZero(h.rows)
	ext.RowsAffected = chUint64OrZero(h.rowsAffected)
	ext.CPUTimeUs = chUint64OrZero(h.cpuTime)
	ext.Memory = chInt64OrZero(h.memory)
	ext.MemoryPeak = chInt64OrZero(h.memoryPeak)
	ext.InBytes = chUint64OrZero(h.inBytes)
	ext.OutBytes = chUint64OrZero(h.outBytes)
	ext.CallWaitUs = chUint64OrZero(h.callWait)
	ext.IfaceName = h.iName
	ext.MethodName = h.mName
	ext.FuncName = h.funcName
	ext.Module = h.module

	ext.Context = h.context
	if h.context != "" {
		ext.ContextHash = city.CH64([]byte(h.context))
	}
	ext.ContextLine = contextLine(h.context, ctxSmart)

	// Первый непустой из Sql | Query | Sdbl
	switch {
	case h.sql != "":
		ext.SQLText = h.sql
	case h.query != "":
		ext.SQLText = h.query
	default:
		ext.SQLText = h.sdbl
	}
	if ext.SQLText != "" {
		ext.SQLHash = city.CH64([]byte(ext.SQLText))
		if norm != nil {
			// Норм-текст не хранится (восстановим нормализатором из sql_text);
			// в строку уходят только хэш нормы, значения и их число.
			normText, params := norm.Normalize(ext.SQLText)
			ext.SQLNormHash = city.CH64(normText)
			ext.SQLParams = params
			if len(params) > math.MaxUint16 {
				metrics.SQLNormSaturated()
				ext.ParamCount = math.MaxUint16
			} else {
				ext.ParamCount = uint16(len(params))
			}
		}
	}
	ext.PlanText = h.planSQLText

	// Первый непустой из Descr | Txt | txt
	switch {
	case h.descr != "":
		ext.Descr = h.descr
	case h.txtU != "":
		ext.Descr = h.txtU
	default:
		ext.Descr = h.txtL
	}
	ext.Exception = h.exception

	ext.LockRegions = h.regions
	ext.LockWaitConns = parseWaitConns(h.waitConnections)
	ext.LocksDump = h.locks
	ext.DeadlockGraph = h.deadlk

	*h = richHot{} // сброс скретча под следующее событие
}

// richEventTime — момент события по правилам импортёра: строка
// "20YY-MM-DDTHH:MM:SS.ssssss" проходит parseDateTime64BestEffortOrNull,
// который ОТВЕРГАЕТ невозможные даты (месяц 13, 30 февраля, час 24,
// минуту/секунду 60) — в отличие от EventTime bench-пути, где time.Date
// нормализует переносом. Невалидная дата/деградированный префикс → эпоха.
func richEventTime(datePrefix string, timePart []byte) time.Time {
	epoch := time.Unix(0, 0).UTC()
	if len(datePrefix) != 14 || len(timePart) != 12 {
		return epoch
	}
	year := atoi4(datePrefix[0:4])
	month := atoi2(datePrefix[5:7])
	day := atoi2(datePrefix[8:10])
	hour := atoi2(datePrefix[11:13])
	min := atoi2b(timePart[0:2])
	sec := atoi2b(timePart[3:5])
	if year < 0 || month < 1 || month > 12 || day < 1 || hour < 0 || hour > 23 ||
		min < 0 || min > 59 || sec < 0 || sec > 59 {
		return epoch
	}
	if day > daysInMonth(year, month) {
		return epoch
	}
	micros := 0
	for _, c := range timePart[6:12] {
		if c < '0' || c > '9' {
			return epoch
		}
		micros = micros*10 + int(c-'0')
	}
	return time.Date(year, time.Month(month), day, hour, min, sec, micros*1000, time.UTC)
}

func daysInMonth(y, m int) int {
	switch m {
	case 1, 3, 5, 7, 8, 10, 12:
		return 31
	case 4, 6, 9, 11:
		return 30
	default: // февраль
		if y%4 == 0 && (y%100 != 0 || y%400 == 0) {
			return 29
		}
		return 28
	}
}
