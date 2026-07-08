# Генератор живого техжурнала для tail-тестов (bakeoff-protocol.md §4.5).
#
#   .\generator.ps1 -Dir <каталог> -Rate 10000 -DurationSec 30
#
# Пишет события в <Dir>\rphost_1\<YYMMDDHH>.log (имя = текущий час, как у реального ТЖ):
#   MM:SS.ssssss-<dur>,CALL,1,Usr=<MarkerPrefix><counter>,GenMs=<unix_ms>,Memory=<n>
# каждое 500-е событие дополнительно несёт многострочный Context='...' на 3 строки.
# Начало каждого файла — UTF-8 BOM (как у реального ТЖ), строки CRLF.
#
# Свойства-инварианты:
#   - файл открыт .NET StreamWriter'ом с FileShare.Read НА ВЕСЬ ПРОГОН
#     (одновременно это и sharing-тест §4.5-7: агент обязан открывать
#     файл с полным набором share-флагов и не требовать себе Write);
#   - FileMode.Create: существующий файл того же часа УСЕКАЕТСЯ до 0
#     (используется сценарием S4 как «усечение на живом агенте»);
#   - пачки пишутся по тику 100 мс + Flush (видимость для читателя);
#   - -RotateAfterSec N: каждые N секунд текущий файл закрывается и
#     открывается файл следующего часа (ротация ТЖ);
#   - сайдкар <Dir>\timeline.csv: counter,unix_ms каждого 1000-го события
#     (+ последнего) — для расчёта латентности;
#   - общее число событий = Rate * DurationSec, ровно; темп выравнивается
#     по монотонным часам, финальный Flush обязателен.
#
# stdout: ровно одна строка JSON со сводкой:
#   {"total":..,"first":..,"last":..,"target_rate":..,"actual_rate":..,"duration_s":..,"files":[..]}
# Человекочитаемая сводка уходит через Write-Host (в пайплайн не попадает).

param(
    [Parameter(Mandatory = $true)][string]$Dir,
    [ValidateRange(1, 1000000)][int]$Rate = 10000,
    [ValidateRange(1, 86400)][int]$DurationSec = 10,
    [long]$StartCounter = 1,
    [ValidateRange(0, 86400)][int]$RotateAfterSec = 0,
    [string]$MarkerPrefix = 'tail_'
)

$ErrorActionPreference = 'Stop'
Set-StrictMode -Version 2.0

if (-not [System.IO.Path]::IsPathRooted($Dir)) {
    $Dir = Join-Path (Get-Location).Path $Dir
}
New-Item -ItemType Directory -Force -Path $Dir | Out-Null

# --- Горячий цикл на C# (PowerShell 5.1 не тянет 20k+ соб/с сам) ---------------
if (-not ('TjTailGen' -as [type])) {
    $csSrc = @'
using System;
using System.Collections.Generic;
using System.Diagnostics;
using System.Globalization;
using System.IO;
using System.Text;
using System.Threading;

public static class TjTailGen
{
    // Возвращает однострочный JSON со сводкой прогона.
    public static string Run(string dir, int rate, int durationSec, long startCounter,
                             int rotateAfterSec, string markerPrefix)
    {
        string procDir = Path.Combine(dir, "rphost_1");
        Directory.CreateDirectory(procDir);

        long total   = (long)rate * (long)durationSec;
        long counter = startCounter;
        long written = 0;

        DateTime fileHour = DateTime.Now;              // имя файла = текущий час
        List<string> files = new List<string>();
        UTF8Encoding bomEnc = new UTF8Encoding(true);  // BOM в позиции 0 каждого файла

        StreamWriter w = OpenLog(procDir, fileHour, bomEnc, files);

        string tlPath = Path.Combine(dir, "timeline.csv");
        StreamWriter tl = new StreamWriter(
            new FileStream(tlPath, FileMode.Create, FileAccess.Write, FileShare.Read),
            new UTF8Encoding(false));
        tl.Write("counter,unix_ms\r\n");
        tl.Flush();

        StringBuilder sb = new StringBuilder(1 << 20);
        Stopwatch sw = Stopwatch.StartNew();
        long rotateStepMs = (long)rotateAfterSec * 1000L;
        long nextRotateMs = rotateAfterSec > 0 ? rotateStepMs : long.MaxValue;

        while (written < total)
        {
            Thread.Sleep(100);                          // тик пачки
            long elapsed = sw.ElapsedMilliseconds;

            if (elapsed >= nextRotateMs)
            {
                w.Flush(); w.Close();                   // ротация: следующий час
                fileHour = fileHour.AddHours(1);
                w = OpenLog(procDir, fileHour, bomEnc, files);
                nextRotateMs += rotateStepMs;
            }

            long due = elapsed * rate / 1000L;          // сколько должно быть к этому мигу
            if (due > total) due = total;
            if (due <= written) continue;

            DateTime now = DateTime.Now;
            string ts   = now.ToString("mm:ss.ffffff", CultureInfo.InvariantCulture);
            long genMs  = UnixMs();

            sb.Length = 0;
            for (long i = written; i < due; i++)
            {
                long c = counter;
                sb.Append(ts).Append('-').Append(1000 + (c % 9000))
                  .Append(",CALL,1,Usr=").Append(markerPrefix).Append(c)
                  .Append(",GenMs=").Append(genMs)
                  .Append(",Memory=").Append(c % 100000);
                if (c % 500 == 0)
                {
                    // многострочный кавычечный Context на 3 строки; строки-продолжения
                    // не совпадают с маской начала события
                    sb.Append(",Context='CommonModule.TailTest : line one ")
                      .Append(markerPrefix).Append(c)
                      .Append("\r\nServerCall.Process : line two")
                      .Append("\r\nForm.Handler : line three'");
                }
                sb.Append("\r\n");
                if (c % 1000 == 0)
                {
                    tl.Write(c); tl.Write(','); tl.Write(genMs); tl.Write("\r\n");
                    tl.Flush();
                }
                counter++;
            }
            written = due;
            w.Write(sb.ToString());
            w.Flush();                                  // видимость для читателя
        }

        w.Flush(); w.Close();
        tl.Write(counter - 1); tl.Write(','); tl.Write(UnixMs()); tl.Write("\r\n");
        tl.Flush(); tl.Close();

        double secs   = sw.ElapsedMilliseconds / 1000.0;
        double actual = secs > 0 ? written / secs : 0.0;

        StringBuilder js = new StringBuilder();
        js.Append("{\"total\":").Append(written)
          .Append(",\"first\":").Append(startCounter)
          .Append(",\"last\":").Append(counter - 1)
          .Append(",\"target_rate\":").Append(rate)
          .Append(",\"actual_rate\":").Append(actual.ToString("F1", CultureInfo.InvariantCulture))
          .Append(",\"duration_s\":").Append(secs.ToString("F3", CultureInfo.InvariantCulture))
          .Append(",\"files\":[");
        for (int i = 0; i < files.Count; i++)
        {
            if (i > 0) js.Append(',');
            js.Append('"').Append(files[i].Replace("\\", "\\\\")).Append('"');
        }
        js.Append("]}");
        return js.ToString();
    }

    private static long UnixMs()
    {
        return (long)(DateTime.UtcNow - new DateTime(1970, 1, 1, 0, 0, 0, DateTimeKind.Utc)).TotalMilliseconds;
    }

    private static StreamWriter OpenLog(string procDir, DateTime hour, UTF8Encoding enc, List<string> files)
    {
        string path = Path.Combine(procDir, hour.ToString("yyMMddHH", CultureInfo.InvariantCulture) + ".log");
        // FileShare.Read: держим запись, читателям разрешено только чтение.
        // FileMode.Create: файл того же часа усекается до 0 (сценарий S4).
        FileStream fs = new FileStream(path, FileMode.Create, FileAccess.Write, FileShare.Read);
        files.Add(path);
        return new StreamWriter(fs, enc);
    }
}
'@
    Add-Type -TypeDefinition $csSrc -Language CSharp
}

$json = [TjTailGen]::Run($Dir, $Rate, $DurationSec, $StartCounter, $RotateAfterSec, $MarkerPrefix)
$sum = $json | ConvertFrom-Json
Write-Host ("генератор: {0} событий ({1}..{2}), целевой темп {3}/с, фактический {4}/с, {5} с, файлов: {6}" -f `
    $sum.total, "$MarkerPrefix$($sum.first)", "$MarkerPrefix$($sum.last)", $sum.target_rate, $sum.actual_rate, $sum.duration_s, $sum.files.Count)
$json
