# Гигиена окружения перед серией замеров (запускать от администратора).
# Протокол: docs/bakeoff-protocol.md §2.3. После замеров вернуть как было!

$ErrorActionPreference = 'Continue'

# Схема электропитания: High performance
powercfg /setactive 8c5e7fda-e8bf-4a96-9a85-a6e23a8c635c

# Windows Defender: исключить корпус и рабочие каталоги из real-time сканирования
Add-MpPreference -ExclusionPath 'E:\TJ_Logs', 'E:\bench', 'E:\git\ТехЖурнал'

Write-Host 'Готово. Вручную: закрыть браузеры/IDE, приостановить Windows Update,'
Write-Host 'не трогать машину во время прогонов. Прогрев кэша: warmup.ps1.'
