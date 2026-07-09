package sqlnorm

import (
	"strings"
	"testing"
)

// fixture — реальный (обрезанный и обезличенный) текст корпуса + ожидаемые
// норма и параметры. Значения строковых литералов заменены на нейтральные,
// структура (кавычки, экранирование, хвосты p_N, регистр) сохранена как в
// живых данных tj.events (см. docs/sql-normalization.md §2).
type fixture struct {
	name   string
	in     string
	norm   string
	params []string
	// hasTail: значения приходят из хвоста p_N — мутации пробелов внутри
	// хвоста меняют захваченные значения, поэтому такие фикстуры исключаются
	// из пробельных мутаций свойства P5.
	hasTail bool
}

var fixtures = []fixture{
	// --- доминирующий шаблон корпуса: параметризованное тело + хвост p_N ---
	{
		name: "point_select_tail_hex",
		in: "SELECT\nT2._IDRRef,\nT2._Version\nFROM dbo._Reference20832 T2\n" +
			"WHERE T2._IDRRef = ? AND T2._Version <> ?\n" +
			"p_0: 0x8145A8494DAAE3A311EE9A7F33D24A23\np_1: 0x0000000287DD3205\n",
		norm:    "SELECT T2._IDRRef, T2._Version FROM dbo._Reference20832 T2 WHERE T2._IDRRef = ? AND T2._Version <> ?",
		params:  []string{"0x8145A8494DAAE3A311EE9A7F33D24A23", "0x0000000287DD3205"},
		hasTail: true,
	},
	{
		name: "update_mixed_tail",
		in: "UPDATE T1 SET _Marked = ?, _Number = ?, _Posted = ?, _Fld15322 = ?\n" +
			"FROM dbo._Document15317 T1\nWHERE T1._IDRRef = ? AND T1._Version = ?\n" +
			"p_0: FALSE\np_1: '008089765'\np_2: TRUE\np_3: 17530101000000\n" +
			"p_4: 0x952C7F30ABC5E95340EA7F09FE17142F\np_5: 0x0000000287DD3205\n",
		norm: "UPDATE T1 SET _Marked = ?, _Number = ?, _Posted = ?, _Fld15322 = ? " +
			"FROM dbo._Document15317 T1 WHERE T1._IDRRef = ? AND T1._Version = ?",
		params: []string{"FALSE", "008089765", "TRUE", "17530101000000",
			"0x952C7F30ABC5E95340EA7F09FE17142F", "0x0000000287DD3205"},
		hasTail: true,
	},
	{
		name:    "in_list_of_placeholders_tail",
		in:      "WHERE T1._Fld123RRef IN (?, ?, ?, ?, ?)\np_0: 0x01\np_1: 0x02\np_2: 0x03\np_3: 0x04\np_4: 0x05\n",
		norm:    "WHERE T1._Fld123RRef IN (?)",
		params:  []string{"0x01", "0x02", "0x03", "0x04", "0x05"},
		hasTail: true,
	},
	{
		name: "insert_values_row_tail",
		in: "INSERT INTO dbo._InfoRg26362 (_Period,_Fld26363,_Fld26364RRef) VALUES(?,?,?)\n" +
			"p_0: 20251129235959\np_1: 0x0A\np_2: ''\n",
		norm:    "INSERT INTO dbo._InfoRg26362 (_Period,_Fld26363,_Fld26364RRef) VALUES(?)",
		params:  []string{"20251129235959", "0x0A", ""},
		hasTail: true,
	},
	{
		name:    "tail_numeric_N_suffix",
		in:      "SELECT x FROM t WHERE a = ? AND b = ?\np_0: 0N\np_1: 123.45N\n",
		norm:    "SELECT x FROM t WHERE a = ? AND b = ?",
		params:  []string{"0N", "123.45N"},
		hasTail: true,
	},
	{
		name:    "tail_multiline_string",
		in:      "SELECT x FROM t WHERE a = ? AND b = ?\np_0: 'стр1\nстр2 ''ок'''\np_1: 42N\n",
		norm:    "SELECT x FROM t WHERE a = ? AND b = ?",
		params:  []string{"стр1\nстр2 'ок'", "42N"},
		hasTail: true,
	},
	{
		name:    "tail_missing_values",
		in:      "SELECT ?, ? FROM t\np_0: 1\n",
		norm:    "SELECT ?, ? FROM t",
		params:  []string{"1", "?"},
		hasTail: true,
	},
	{
		name:    "tail_extra_values_dropped",
		in:      "SELECT ?\np_0: 1\np_1: 2\n",
		norm:    "SELECT ?",
		params:  []string{"1"},
		hasTail: true,
	},
	{
		name:    "tail_only_degenerate",
		in:      "p_0: 5\n",
		norm:    "",
		params:  []string{},
		hasTail: true,
	},
	{
		name:    "tail_crlf_lines",
		in:      "SELECT x FROM t WHERE a = ?\r\np_0: 42\r\n",
		norm:    "SELECT x FROM t WHERE a = ?",
		params:  []string{"42"},
		hasTail: true,
	},

	// --- inline-литералы без хвоста ---
	{
		name: "tt_insert_hex_inline",
		in: "INSERT INTO #tt193 WITH(TABLOCK) (_Q_000_F_000, _Q_000_F_001RRef) SELECT\n" +
			"T1._Period,\nT1._Fld34674RRef\nFROM dbo._InfoRg34673 T1\nWHERE (T1._Fld34679 = 0x00)",
		norm: "INSERT INTO #tt WITH(TABLOCK) (_Q_000_F_000, _Q_000_F_001RRef) SELECT " +
			"T1._Period, T1._Fld34674RRef FROM dbo._InfoRg34673 T1 WHERE (T1._Fld34679 = ?)",
		params: []string{"0x00"},
	},
	{
		name: "case_hex_chain",
		in:   "CASE WHEN T3._Fld21497_TYPE = 0x08 THEN 0x000086D2 ELSE T3._Fld21497_TYPE END",
		norm: "CASE WHEN T3._Fld21497_TYPE = ? THEN ? ELSE T3._Fld21497_TYPE END",
		params: []string{
			"0x08", "0x000086D2",
		},
	},
	{
		name: "in_nstrings_numeric_precision",
		in: "CAST(SUM(CASE WHEN (T18._Fld12828 IN (N'Ткань', N'Фурнитура', N'Прочее')) " +
			"THEN T1.Fld14449Turnover_ ELSE 0.0 END) AS NUMERIC(27, 2))",
		norm: "CAST(SUM(CASE WHEN (T18._Fld12828 IN (?)) " +
			"THEN T1.Fld14449Turnover_ ELSE ? END) AS NUMERIC(?, ?))",
		params: []string{"Ткань", "Фурнитура", "Прочее", "0.0", "27", "2"},
	},
	{
		name: "having_decimal_group",
		in: "GROUP BY T4._Fld32435RRef\nHAVING (CAST(SUM(T4._Fld32437) AS NUMERIC(27, 3))) <> 0.0\n" +
			"UNION ALL SELECT TOP 1 x FROM t",
		norm: "GROUP BY T4._Fld32435RRef HAVING (CAST(SUM(T4._Fld32437) AS NUMERIC(?, ?))) <> ? " +
			"UNION ALL SELECT TOP ? x FROM t",
		params: []string{"27", "3", "0.0", "1"},
	},
	{
		name:   "odbc_ts_escape",
		in:     "WHERE (T1._Q_001_F_009 >= {ts '2024-02-15 00:00:00'})",
		norm:   "WHERE (T1._Q_001_F_009 >= {ts ?})",
		params: []string{"2024-02-15 00:00:00"},
	},
	{
		name: "unary_minus_contexts",
		in:   "SELECT CASE WHEN T1.RecordKind = 0.0 THEN T1.F ELSE -T1.F END, (-1.0 * T2.Balance), x - 5, y = -7",
		norm: "SELECT CASE WHEN T1.RecordKind = ? THEN T1.F ELSE -T1.F END, (? * T2.Balance), x - ?, y = ?",
		params: []string{
			"0.0", "-1.0", "5", "-7",
		},
	},
	{
		name:   "string_escapes",
		in:     "x = N'aa''bb' AND y = 'it''s'",
		norm:   "x = ? AND y = ?",
		params: []string{"aa'bb", "it's"},
	},
	{
		name:   "comment_markers_inside_strings",
		in:     "WHERE c = '0104e;B--gim(Er' AND d = 'x/*y*/'",
		norm:   "WHERE c = ? AND d = ?",
		params: []string{"0104e;B--gim(Er", "x/*y*/"},
	},
	{
		name:   "in_subquery_no_collapse",
		in:     "WHERE T6._OwnerIDRRef IN (SELECT T7._Q_001_F_000RRef FROM #tt324 T7 WITH(NOLOCK))",
		norm:   "WHERE T6._OwnerIDRRef IN (SELECT T7._Q_001_F_000RRef FROM #tt T7 WITH(NOLOCK))",
		params: []string{},
	},
	{
		name:   "in_mixed_partial_collapse",
		in:     "WHERE a IN (1, T1.b, 3)",
		norm:   "WHERE a IN (?, T1.b, ?)",
		params: []string{"1", "3"},
	},
	{
		name:   "in_nospace_and_lowercase",
		in:     "WHERE a IN(1,2) AND b in ( 3 , 4 )",
		norm:   "WHERE a IN(?) AND b in ( ? )",
		params: []string{"1", "2", "3", "4"},
	},
	{
		name:   "values_multirow_ctor",
		in:     "INSERT INTO t (a, b) VALUES (1, 'x'), (2, 'y'), (3, 'z')",
		norm:   "INSERT INTO t (a, b) VALUES (?)",
		params: []string{"1", "x", "2", "y", "3", "z"},
	},
	{
		name: "sp_executesql_call",
		in: "{call sp_executesql(N'SELECT F FROM Config WHERE FileName = @P1 ORDER BY PartNo', " +
			"N'@P1 nvarchar(128)', N'DynamicallyUpdated')}",
		norm: "{call sp_executesql(?, ?, ?)}",
		params: []string{
			"SELECT F FROM Config WHERE FileName = @P1 ORDER BY PartNo",
			"@P1 nvarchar(128)", "DynamicallyUpdated",
		},
	},
	{
		name:   "rpc_at_p_blinding",
		in:     "SELECT xtype FROM sysobjects WHERE id = object_id(@P1) OR id = @P2 OR c = @@ROWCOUNT OR v = @Var7",
		norm:   "SELECT xtype FROM sysobjects WHERE id = object_id(@P) OR id = @P OR c = @@ROWCOUNT OR v = @Var7",
		params: []string{},
	},
	{
		name:   "temp_table_hex_names",
		in:     "SELECT a INTO #Te4ea8b9926e04f3ea70f082f16ba89f0 FROM #T633d0475 T1 WHERE n = 5",
		norm:   "SELECT a INTO #Te FROM #T T1 WHERE n = ?",
		params: []string{"5"},
	},
	{
		name: "qerr_1c_language",
		in: "ВЫБРАТЬ ПЕРВЫЕ 1\n\tПокупатели.Ссылка КАК Ссылка,\n\tВЫБОР\n\t\tКОГДА Покупатели.Код ЕСТЬ NULL\n" +
			"\t\t\tТОГДА 0\n\t\tИНАЧЕ 1\n\tКОНЕЦ КАК Поле1\nИЗ Справочник.Покупатели КАК Покупатели\nГДЕ Покупатели.Код = &Код",
		norm: "ВЫБРАТЬ ПЕРВЫЕ ? Покупатели.Ссылка КАК Ссылка, ВЫБОР КОГДА Покупатели.Код ЕСТЬ NULL " +
			"ТОГДА ? ИНАЧЕ ? КОНЕЦ КАК Поле1 ИЗ Справочник.Покупатели КАК Покупатели ГДЕ Покупатели.Код = &Код",
		params: []string{"1", "0", "1"},
	},
	{
		name: "excpcntx_double_quotes",
		in: `SELECT SettingsData FROM InternalSettings WHERE UserIdHash = 723462868 AND ` +
			`ObjectKey = "Common/Tpl" AND UserId LIKE "Ma\_S" ESCAPE "\" AND K LIKE ""`,
		norm: `SELECT SettingsData FROM InternalSettings WHERE UserIdHash = ? AND ` +
			`ObjectKey = "Common/Tpl" AND UserId LIKE "Ma\_S" ESCAPE "\" AND K LIKE ""`,
		params: []string{"723462868"},
	},
	{
		name:   "bracket_identifiers_verbatim",
		in:     "SELECT [T1].[Fld 25343] FROM [tbl]]x] WHERE [T1].[a] = 5",
		norm:   "SELECT [T1].[Fld 25343] FROM [tbl]]x] WHERE [T1].[a] = ?",
		params: []string{"5"},
	},
	{
		name:   "exponents_and_bare_decimal",
		in:     "a = 1e5 AND b = 1E-3 AND c = 2.5e+7 AND d = .5 AND e = 5. AND f = 0XFF",
		norm:   "a = ? AND b = ? AND c = ? AND d = ? AND e = ? AND f = ?",
		params: []string{"1e5", "1E-3", "2.5e+7", ".5", "5.", "0XFF"},
	},
	{
		name:   "whitespace_collapse_trim",
		in:     "  SELECT\t1\r\nFROM   t  ",
		norm:   "SELECT ? FROM t",
		params: []string{"1"},
	},
	{
		name:   "unterminated_string",
		in:     "WHERE a = 'abc",
		norm:   "WHERE a = ?",
		params: []string{"abc"},
	},
	{
		name:   "bare_placeholder_no_tail",
		in:     "SELECT ?",
		norm:   "SELECT ?",
		params: []string{"?"},
	},
	{
		name:   "digits_inside_identifiers_kept",
		in:     "SELECT T2._Fld25768_TYPE, T2._Fld25768_S FROM dbo._AccumRgT32439 T4 WHERE Q_001_F_000 = 1",
		norm:   "SELECT T2._Fld25768_TYPE, T2._Fld25768_S FROM dbo._AccumRgT32439 T4 WHERE Q_001_F_000 = ?",
		params: []string{"1"},
	},
	{
		name:   "empty_input",
		in:     "",
		norm:   "",
		params: []string{},
	},
	{
		name: "exec_sp_executesql_bare",
		in: "exec sp_executesql N'select xtype from sys.sysobjects where id = object_id(@P1)', " +
			"N'@P1 nvarchar(128)', N'ExternalBinDataStrgsList'",
		norm: "exec sp_executesql ?, ?, ?",
		params: []string{
			"select xtype from sys.sysobjects where id = object_id(@P1)",
			"@P1 nvarchar(128)", "ExternalBinDataStrgsList",
		},
	},
	{
		name: "top_offset_fetch",
		in:   "SELECT TOP 25 x FROM t ORDER BY x OFFSET 50 ROWS FETCH NEXT 25 ROWS ONLY",
		norm: "SELECT TOP ? x FROM t ORDER BY x OFFSET ? ROWS FETCH NEXT ? ROWS ONLY",
		params: []string{
			"25", "50", "25",
		},
	},
	{
		name: "deep_alias_join_shape",
		in: "SELECT\nT8._Fld32435RRef AS Fld32435RRef,\nCAST(CAST(SUM(CASE WHEN T8._RecordKind = 0.0 " +
			"THEN T8._Fld32437 ELSE -T8._Fld32437 END) AS NUMERIC(21, 3)) AS NUMERIC(27, 3)) AS Fld32437Balance_\n" +
			"FROM dbo._AccumRg32434 T8\nWHERE T8._Period >= ? AND T8._Period < ? AND T8._Active = 0x01\n" +
			"p_0: 20251101000000\np_1: 20251201000000\n",
		norm: "SELECT T8._Fld32435RRef AS Fld32435RRef, CAST(CAST(SUM(CASE WHEN T8._RecordKind = ? " +
			"THEN T8._Fld32437 ELSE -T8._Fld32437 END) AS NUMERIC(?, ?)) AS NUMERIC(?, ?)) AS Fld32437Balance_ " +
			"FROM dbo._AccumRg32434 T8 WHERE T8._Period >= ? AND T8._Period < ? AND T8._Active = ?",
		params: []string{"0.0", "21", "3", "27", "3",
			"20251101000000", "20251201000000", "0x01"},
		hasTail: true,
	},
}

func TestFixtures(t *testing.T) {
	var n Normalizer
	for _, f := range fixtures {
		t.Run(f.name, func(t *testing.T) {
			norm, params := n.NormalizeString(f.in)
			if norm != f.norm {
				t.Errorf("норма:\n got: %q\nwant: %q", norm, f.norm)
			}
			if len(params) != len(f.params) {
				t.Fatalf("params: got %d %q, want %d %q", len(params), params, len(f.params), f.params)
			}
			for i := range params {
				if params[i] != f.params[i] {
					t.Errorf("params[%d] = %q, want %q", i, params[i], f.params[i])
				}
			}
		})
	}
}

// TestProperties — инварианты нормализации поверх фикстур и их мутаций.
//
//	P1: норма идемпотентна по тексту: normalize(norm).text == norm.text;
//	P2: повторная нормализация не извлекает ни одного ЗНАЧЕНИЯ — только
//	    нераскрытые '?' (в норме не осталось литералов);
//	P3: число '?' в норме ≤ len(params); равенство, когда не было коллапса
//	    списков (спека §4.3: коллапс IN/VALUES сохраняет захват значений);
//	P4: детерминизм повторного прогона (тот же вход → та же норма и params);
//	P5: пробельные мутации тела (удвоение пробелов, \n→\r\n, обрамление)
//	    не меняют ни норму, ни params (для фикстур без хвоста p_N);
//	P6: в норме нет апострофов (все строковые литералы извлечены).
func TestProperties(t *testing.T) {
	var n, n2 Normalizer
	for _, f := range fixtures {
		norm1, params1 := n.NormalizeString(f.in)

		// P1+P2: идемпотентность и отсутствие значений при повторном прогоне
		norm2, params2 := n2.NormalizeString(norm1)
		if norm2 != norm1 {
			t.Errorf("%s: P1 нарушено:\n norm1: %q\n norm2: %q", f.name, norm1, norm2)
		}
		for i, p := range params2 {
			if p != unresolved {
				t.Errorf("%s: P2 нарушено: повторный прогон извлёк значение params[%d]=%q", f.name, i, p)
			}
		}

		// P3: '?' в норме против числа параметров
		q := strings.Count(norm1, "?")
		if q > len(params1) {
			t.Errorf("%s: P3 нарушено: %d '?' в норме > %d params", f.name, q, len(params1))
		}

		// P4: детерминизм
		norm3, params3 := n2.NormalizeString(f.in)
		if norm3 != norm1 || len(params3) != len(params1) {
			t.Errorf("%s: P4 нарушено (повторный прогон дал другой результат)", f.name)
		}
		for i := range params3 {
			if params3[i] != params1[i] {
				t.Errorf("%s: P4 нарушено: params[%d] %q != %q", f.name, i, params3[i], params1[i])
			}
		}

		// P6: строковые литералы извлечены полностью
		if strings.ContainsRune(norm1, '\'') {
			t.Errorf("%s: P6 нарушено: в норме остался апостроф: %q", f.name, norm1)
		}

		// P5: пробельные мутации тела. Применимы только там, где мутация не
		// задевает содержимое литералов: строки '...' и скобочные [идентификаторы]
		// сохраняют пробелы как есть, поэтому фикстуры с ними исключаются
		// (как и хвосты p_N — их значения захватываются как написано).
		if f.hasTail || f.in == "" || strings.ContainsAny(f.in, "'[") {
			continue
		}
		mutations := []string{
			strings.ReplaceAll(f.in, " ", "  "),
			strings.ReplaceAll(f.in, "\n", "\r\n"),
			" \t" + f.in + " \n",
		}
		for mi, m := range mutations {
			nm, pm := n2.NormalizeString(m)
			if nm != norm1 {
				t.Errorf("%s: P5 нарушено (мутация %d):\n got: %q\nwant: %q", f.name, mi, nm, norm1)
			}
			if len(pm) != len(params1) {
				t.Errorf("%s: P5 нарушено (мутация %d): params %d != %d", f.name, mi, len(pm), len(params1))
				continue
			}
			for i := range pm {
				if pm[i] != params1[i] {
					t.Errorf("%s: P5 нарушено (мутация %d): params[%d] %q != %q", f.name, mi, i, pm[i], params1[i])
				}
			}
		}
	}
}

// TestScratchReuse — скретч нормализатора чист между вызовами: короткий
// текст после длинного не наследует ни хвостов, ни позиций '?'.
func TestScratchReuse(t *testing.T) {
	var n Normalizer
	_, _ = n.NormalizeString("SELECT ?, ?, ? FROM t\np_0: 1\np_1: 2\np_2: 3\n")
	norm, params := n.NormalizeString("SELECT ?")
	if norm != "SELECT ?" || len(params) != 1 || params[0] != "?" {
		t.Errorf("скретч грязный: %q %q", norm, params)
	}
}

// TestNormBufferReuse — контракт буфера: норма валидна до следующего вызова.
func TestNormBufferReuse(t *testing.T) {
	var n Normalizer
	b1, _ := n.Normalize("SELECT 1")
	s1 := string(b1) // снять копию до следующего вызова
	b2, _ := n.Normalize("SELECT 2, 3")
	if s1 != "SELECT ?" || string(b2) != "SELECT ?, ?" {
		t.Errorf("нормы: %q, %q", s1, string(b2))
	}
}

// TestLongInList — длинный IN-список (реальный корпус: до 730 значений):
// норма схлопывается, счётчик сохраняет длину.
func TestLongInList(t *testing.T) {
	var sb strings.Builder
	sb.WriteString("SELECT x FROM t WHERE id IN (")
	for i := 0; i < 730; i++ {
		if i > 0 {
			sb.WriteString(", ")
		}
		sb.WriteString("?")
	}
	sb.WriteString(")\n")
	for i := 0; i < 730; i++ {
		sb.WriteString("p_")
		sb.WriteString(itoa(i))
		sb.WriteString(": 0x0")
		sb.WriteString(itoa(i % 10))
		sb.WriteString("\n")
	}
	var n Normalizer
	norm, params := n.NormalizeString(sb.String())
	if norm != "SELECT x FROM t WHERE id IN (?)" {
		t.Errorf("норма: %q", norm)
	}
	if len(params) != 730 {
		t.Errorf("params: %d, want 730", len(params))
	}
	if params[0] != "0x00" || params[729] != "0x09" {
		t.Errorf("края: %q, %q", params[0], params[729])
	}
}

func itoa(v int) string {
	if v == 0 {
		return "0"
	}
	var b [8]byte
	i := len(b)
	for v > 0 {
		i--
		b[i] = byte('0' + v%10)
		v /= 10
	}
	return string(b[i:])
}

// benchInput — вход ~1 КБ (медиана корпуса 277 Б, p99 ~11 КБ): реальная
// форма DBMSSQL — тело с '?', inline-литералами и хвостом p_N.
var benchInput = "SELECT\nT2._IDRRef,\nT2._Version,\nT2._Marked,\nT2._PredefinedID,\nT2._OwnerIDRRef,\nT2._Code,\n" +
	"T2._Fld20837,\nT2._Fld20838RRef,\nT2._Fld23555,\nT2._Fld23719,\nT2._Fld24307,\nT2._Fld25343,\n" +
	"T2._Fld25712,\nT2._Fld25713,\nT2._Fld25714RRef,\nT2._Fld25715RRef,\nT2._Fld25716RRef,\n" +
	"T2._Fld25717RRef,\nT2._Fld25718,\nT2._Fld25719,\nT2._Fld25768_TYPE,\nT2._Fld25768_S,\n" +
	"T2._Fld25768_RTRef,\nT2._Fld25768_RRRef,\nT2._Fld25720,\nT2._Fld25767RRef,\nT2._Fld25769,\n" +
	"T2._Fld34197RRef,\nT2._Fld39877\nFROM dbo._Reference20832 T2\n" +
	"WHERE T2._IDRRef IN (?, ?, ?, ?) AND T2._Version <> ? AND T2._Marked = 0x00 AND " +
	"T2._Code IN (N'А00-123', N'Б00-456') AND T2._Fld25719 > 100.5\n" +
	"p_0: 0x8145A8494DAAE3A311EE9A7F33D24A23\np_1: 0x8145A8494DAAE3A311EE9A7F33D24A24\n" +
	"p_2: 0x8145A8494DAAE3A311EE9A7F33D24A25\np_3: 0x8145A8494DAAE3A311EE9A7F33D24A26\n" +
	"p_4: 0x0000000287DD3205\n"

func BenchmarkNormalize1KB(b *testing.B) {
	var n Normalizer
	b.SetBytes(int64(len(benchInput)))
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_, params := n.Normalize(benchInput)
		if len(params) != 9 {
			b.Fatalf("params: %d", len(params))
		}
	}
}

// BenchmarkNormalizeMedian277B — типичный короткий текст (медиана корпуса).
func BenchmarkNormalizeMedian277B(b *testing.B) {
	in := "SELECT\nT2._IDRRef,\nT2._Version,\nT2._Code\nFROM dbo._Reference20832 T2\n" +
		"WHERE T2._IDRRef = ? AND T2._Version <> ? AND T2._Marked = 0x00 AND T2._Fld25719 > 100.5\n" +
		"p_0: 0x8145A8494DAAE3A311EE9A7F33D24A23\np_1: 0x0000000287DD3205\n"
	var n Normalizer
	b.SetBytes(int64(len(in)))
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_, _ = n.Normalize(in)
	}
}

// BenchmarkNormalizeBig3MB — потолок корпуса (максимальный текст 3.1 МБ):
// линейность одного прохода.
func BenchmarkNormalizeBig3MB(b *testing.B) {
	var sb strings.Builder
	for sb.Len() < 3<<20 {
		sb.WriteString(benchInput)
	}
	in := sb.String()
	var n Normalizer
	b.SetBytes(int64(len(in)))
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_, _ = n.Normalize(in)
	}
}
