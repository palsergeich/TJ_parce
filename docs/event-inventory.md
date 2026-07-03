# 1C Technological Journal — Event Inventory

Generated 2026-07-03 from E:\TJ_Logs\TJ_Logs. Small collections processed fully; big collections sampled via one moderate rphost_* subfolder each.

## Collections processed

| Collection | Raw MB processed | Events | Events/MB | Full size MB | Est. total events |
|---|---:|---:|---:|---:|---:|
| CallsDiag | 72.7 | 120,961 | 1,664 | 3,072 | 5,111,309 |
| CallsDiag_86 | 8.7 | 21,112 | 2,427 | 9 | 21,840 |
| Diag | 153.4 | 569,902 | 3,715 | 33,792 | 125,541,906 |
| Diag_86 | 271.7 | 1,256,398 | 4,624 | 273 | 1,262,409 |
| EXP | 37.7 | 79,153 | 2,100 | 538 | 1,129,557 |
| EXP_86 | 7.3 | 30,790 | 4,218 | 8 | 33,742 |
| Lic | 44.5 | 74,382 | 1,672 | 47 | 78,560 |
| Lic_86 | 1.3 | 3,343 | 2,572 | 2 | 5,143 |
| LongDB_01 | 228.3 | 21,198 | 93 | 36,864 | 3,422,878 |
| LongDB_01_86 | 0.1 | 26 | 371 | 1 | 371 |
| Mem | 76.9 | 103,959 | 1,352 | 30,720 | 41,529,525 |
| QERR_Diag | 156.5 | 14,532 | 93 | 19,456 | 1,806,610 |
| TLockDiag | 358.9 | 22,018 | 61 | 361 | 22,146 |
| _AccumRgTn14466_AccumRg14438 | 198.6 | 25,287 | 127 | 8,192 | 1,043,056 |
| _Document15317 | 210.3 | 79,614 | 379 | 212 | 80,257 |
| _Reference20832 | 248.2 | 65,675 | 265 | 43,008 | 11,380,138 |
| _Reference27041 | 68.7 | 32,979 | 480 | 1,024 | 491,564 |
| _ReferenceChngR12527 | 303.3 | 63,768 | 210 | 305 | 64,125 |

**Estimated total events in full 175 GB corpus: ~193,025,136**

## Event type summary

| Event | Count | Collections | Avg duration (us) | Max duration (us) | # distinct props |
|---|---:|---|---:|---:|---:|
| CONN | 1,310,297 | Diag, Diag_86 | 14,296,231 | 17,543,573,004 | 24 |
| CLSTR | 272,420 | Diag, Diag_86 | 0 | 0 | 42 |
| DBMSSQL | 255,277 | LongDB_01, LongDB_01_86, _AccumRgTn14466_AccumRg14438, _Document15317, _Reference20832, _Reference27041, _ReferenceChngR12527 | 66,933 | 86,516,899 | 29 |
| CALL | 181,286 | CallsDiag, CallsDiag_86, Mem | 1,624,231 | 5,700,483,937 | 29 |
| EXCP | 143,092 | CallsDiag, Diag, Diag_86, EXP, EXP_86, Lic, LongDB_01, Mem, QERR_Diag, TLockDiag, _AccumRgTn14466_AccumRg14438, _Document15317, _Reference20832, _Reference27041, _ReferenceChngR12527 | 0 | 0 | 21 |
| ATTN | 142,994 | Diag_86 | 0 | 0 | 16 |
| Context | 81,420 | CallsDiag, Diag, Diag_86, EXP_86, Lic, LongDB_01, Mem, TLockDiag, _AccumRgTn14466_AccumRg14438, _Document15317, _Reference20832 | 0 | 0 | 11 |
| LIC | 66,355 | Lic, Lic_86 | 3,040 | 12,109,946 | 12 |
| EXCPCNTX | 55,104 | Diag, Diag_86, EXP, EXP_86 | 171,222 | 3,600,358,894 | 37 |
| SESN | 36,565 | Diag_86 | 16,423,299 | 4,203,981,094 | 12 |
| QERR | 20,610 | Diag, Diag_86, QERR_Diag | 0 | 0 | 15 |
| TLOCK | 10,357 | TLockDiag | 2,940,748 | 20,017,727 | 20 |
| SCOM | 5,525 | Diag, Diag_86 | 44,381,618 | 17,544,979,244 | 17 |
| ADMIN | 1,595 | Diag, Diag_86 | 0 | 0 | 15 |
| SCALL | 1,182 | CallsDiag, CallsDiag_86 | 215,118 | 2,359,982 | 20 |
| PROC | 722 | Diag, Diag_86 | 129 | 15,976 | 12 |
| TTIMEOUT | 264 | TLockDiag | 0 | 0 | 14 |
| TDEADLOCK | 32 | TLockDiag | 0 | 0 | 13 |

## CONN

Count: 1,310,297. Collections: Diag, Diag_86. Avg duration 14,296,231 us, max 17,543,573,004 us.

| Property | Present | Presence % | Type |
|---|---:|---:|---|
| process | 1,310,297 | 100.0 | string |
| OSThread | 1,310,297 | 100.0 | number |
| Txt | 1,085,131 | 82.8 | string |
| p:processName | 485,260 | 37.0 | string |
| ClientID | 429,783 | 32.8 | number |
| t:clientID | 244,675 | 18.7 | number |
| Protected | 162,421 | 12.4 | number |
| t:applicationName | 149,765 | 11.4 | string |
| t:computerName | 120,101 | 9.2 | string |
| t:connectID | 119,501 | 9.1 | number |
| Calls | 119,435 | 9.1 | number |
| Descr | 105,731 | 8.1 | string |
| lka | 3,023 | 0.2 | number |
| lkaid | 3,023 | 0.2 | string |
| lkato | 3,023 | 0.2 | number |
| lkp | 3,023 | 0.2 | number |
| lkpid | 3,023 | 0.2 | mixed (num 93, str 2930) |
| lksrc | 3,023 | 0.2 | mixed (num 93, str 2930) |
| lkpto | 3,023 | 0.2 | number |
| Usr | 59 | 0.0 | string |
| SessionID | 30 | 0.0 | number |
| DBMS | 24 | 0.0 | string |
| DataBase | 24 | 0.0 | string |
| Context | 20 | 0.0 | string |

## CLSTR

Count: 272,420. Collections: Diag, Diag_86. Avg duration 0 us, max 0 us.

| Property | Present | Presence % | Type |
|---|---:|---:|---|
| process | 272,420 | 100.0 | string |
| OSThread | 272,420 | 100.0 | number |
| Event | 272,420 | 100.0 | string |
| p:processName | 271,445 | 99.6 | string |
| t:clientID | 252,785 | 92.8 | number |
| t:applicationName | 252,785 | 92.8 | string |
| t:computerName | 252,785 | 92.8 | string |
| RmngrURL | 170,609 | 62.6 | string |
| ServiceName | 170,609 | 62.6 | string |
| TargetCall | 170,609 | 62.6 | number |
| t:connectID | 169,612 | 62.3 | number |
| InfoBase | 169,339 | 62.2 | string |
| SessionID | 169,134 | 62.1 | mixed (num 695, str 168439) |
| Data | 86,912 | 31.9 | string |
| ExtData | 84,021 | 30.8 | string |
| Usr | 75,470 | 27.7 | string |
| Ref | 10,336 | 3.8 | string |
| SrcAddr | 10,336 | 3.8 | string |
| SrcId | 10,336 | 3.8 | string |
| SrcPid | 10,336 | 3.8 | mixed (num 3092, str 7244) |
| ApplicationExt | 10,336 | 3.8 | string |
| Request | 7,496 | 2.8 | string |
| DstAddr | 7,486 | 2.7 | string |
| DstId | 7,486 | 2.7 | string |
| DstPid | 7,486 | 2.7 | mixed (num 7456, str 30) |
| ByTime | 3,208 | 1.2 | string |
| Message | 813 | 0.3 | string |
| DistribData | 545 | 0.2 | string |
| Context | 271 | 0.1 | string |
| Txt | 237 | 0.1 | string |
| DBMS | 194 | 0.1 | string |
| DataBase | 191 | 0.1 | string |
| DstSrv | 124 | 0.0 | string |
| Obsolete | 103 | 0.0 | string |
| Registered | 102 | 0.0 | string |
| AppID | 100 | 0.0 | string |
| Host | 70 | 0.0 | string |
| Connections | 70 | 0.0 | number |
| Infobases | 70 | 0.0 | number |
| Deficit | 70 | 0.0 | number |
| procURL | 32 | 0.0 | string |
| Released | 30 | 0.0 | string |

## DBMSSQL

Count: 255,277. Collections: LongDB_01, LongDB_01_86, _AccumRgTn14466_AccumRg14438, _Document15317, _Reference20832, _Reference27041, _ReferenceChngR12527. Avg duration 66,933 us, max 86,516,899 us.

| Property | Present | Presence % | Type |
|---|---:|---:|---|
| process | 255,277 | 100.0 | string |
| p:processName | 255,277 | 100.0 | string |
| OSThread | 255,277 | 100.0 | number |
| t:clientID | 255,277 | 100.0 | number |
| t:computerName | 255,277 | 100.0 | string |
| t:connectID | 255,277 | 100.0 | number |
| DBMS | 255,277 | 100.0 | string |
| DataBase | 255,277 | 100.0 | string |
| Trans | 255,277 | 100.0 | number |
| SessionID | 255,276 | 100.0 | number |
| Usr | 255,276 | 100.0 | string |
| dbpid | 255,269 | 100.0 | number |
| Sql | 255,269 | 100.0 | string |
| RowsAffected | 255,239 | 100.0 | number |
| t:applicationName | 255,235 | 100.0 | string |
| planSQLText | 255,227 | 100.0 | string |
| Rows | 249,512 | 97.7 | number |
| Context | 237,874 | 93.2 | string |
| AppID | 20,995 | 8.2 | string |
| Prm | 5,726 | 2.2 | string |
| Func | 8 | 0.0 | string |
| tableName | 4 | 0.0 | string |
| lkp | 3 | 0.0 | number |
| lkpid | 3 | 0.0 | number |
| lksrc | 3 | 0.0 | number |
| lkpto | 3 | 0.0 | number |
| lka | 2 | 0.0 | number |
| lkaid | 2 | 0.0 | mixed (num 1, str 1) |
| lkato | 2 | 0.0 | number |

## CALL

Count: 181,286. Collections: CallsDiag, CallsDiag_86, Mem. Avg duration 1,624,231 us, max 5,700,483,937 us.

| Property | Present | Presence % | Type |
|---|---:|---:|---|
| process | 181,286 | 100.0 | string |
| OSThread | 181,286 | 100.0 | number |
| t:clientID | 181,286 | 100.0 | number |
| callWait | 181,286 | 100.0 | number |
| first | 181,286 | 100.0 | number |
| Memory | 181,286 | 100.0 | number |
| MemoryPeak | 181,286 | 100.0 | number |
| InBytes | 181,286 | 100.0 | number |
| OutBytes | 181,286 | 100.0 | number |
| CpuTime | 181,286 | 100.0 | number |
| Method | 181,062 | 99.9 | mixed (num 165049, str 16013) |
| p:processName | 179,915 | 99.2 | string |
| SessionID | 169,719 | 93.6 | mixed (num 161384, str 8335) |
| Usr | 169,658 | 93.6 | string |
| CallID | 166,235 | 91.7 | number |
| Interface | 165,049 | 91.0 | string |
| IName | 165,049 | 91.0 | string |
| MName | 165,049 | 91.0 | string |
| t:applicationName | 164,981 | 91.0 | string |
| t:computerName | 164,858 | 90.9 | string |
| t:connectID | 106,942 | 59.0 | number |
| AppID | 57,906 | 31.9 | string |
| Func | 15,020 | 8.3 | string |
| Module | 14,827 | 8.2 | string |
| Context | 14,325 | 7.9 | string |
| Form | 183 | 0.1 | string |
| FormItem | 183 | 0.1 | string |
| SearchString | 183 | 0.1 | string |
| RetExcp | 40 | 0.0 | string |

## EXCP

Count: 143,092. Collections: CallsDiag, Diag, Diag_86, EXP, EXP_86, Lic, LongDB_01, Mem, QERR_Diag, TLockDiag, _AccumRgTn14466_AccumRg14438, _Document15317, _Reference20832, _Reference27041, _ReferenceChngR12527. Avg duration 0 us, max 0 us.

| Property | Present | Presence % | Type |
|---|---:|---:|---|
| process | 143,092 | 100.0 | string |
| OSThread | 143,092 | 100.0 | number |
| Exception | 136,686 | 95.5 | string |
| Descr | 134,742 | 94.2 | string |
| p:processName | 78,525 | 54.9 | string |
| t:clientID | 62,778 | 43.9 | number |
| t:applicationName | 62,777 | 43.9 | string |
| t:computerName | 62,777 | 43.9 | string |
| t:connectID | 59,754 | 41.8 | number |
| ClientID | 54,208 | 37.9 | number |
| SessionID | 26,422 | 18.5 | number |
| Usr | 26,383 | 18.4 | string |
| Context | 7,353 | 5.1 | string |
| exception | 6,406 | 4.5 | string |
| method | 6,406 | 4.5 | string |
| processID | 6,406 | 4.5 | string |
| url | 6,406 | 4.5 | string |
| DBMS | 1,248 | 0.9 | string |
| DataBase | 1,242 | 0.9 | string |
| AppID | 1,028 | 0.7 | string |
| dbpid | 81 | 0.1 | number |

## ATTN

Count: 142,994. Collections: Diag_86. Avg duration 0 us, max 0 us.

| Property | Present | Presence % | Type |
|---|---:|---:|---|
| process | 142,994 | 100.0 | string |
| OSThread | 142,994 | 100.0 | number |
| Descr | 142,994 | 100.0 | string |
| AgentUrl | 124,982 | 87.4 | string |
| Url | 88,965 | 62.2 | string |
| ProcessId | 88,965 | 62.2 | string |
| Pid | 88,965 | 62.2 | number |
| p:processName | 18,012 | 12.6 | string |
| t:clientID | 18,012 | 12.6 | number |
| t:applicationName | 18,012 | 12.6 | string |
| t:computerName | 18,012 | 12.6 | string |
| ServerId | 18,012 | 12.6 | string |
| Host | 18,012 | 12.6 | string |
| FreeMemory | 18,012 | 12.6 | number |
| SafeLimit | 18,012 | 12.6 | number |
| Info | 4 | 0.0 | string |

## Context

Count: 81,420. Collections: CallsDiag, Diag, Diag_86, EXP_86, Lic, LongDB_01, Mem, TLockDiag, _AccumRgTn14466_AccumRg14438, _Document15317, _Reference20832. Avg duration 0 us, max 0 us.

| Property | Present | Presence % | Type |
|---|---:|---:|---|
| process | 81,420 | 100.0 | string |
| p:processName | 81,420 | 100.0 | string |
| OSThread | 81,420 | 100.0 | number |
| t:clientID | 81,420 | 100.0 | number |
| t:computerName | 81,420 | 100.0 | string |
| Context | 81,420 | 100.0 | string |
| t:connectID | 81,397 | 100.0 | number |
| t:applicationName | 81,378 | 99.9 | string |
| SessionID | 80,598 | 99.0 | number |
| Usr | 80,598 | 99.0 | string |
| AppID | 60,001 | 73.7 | string |

## LIC

Count: 66,355. Collections: Lic, Lic_86. Avg duration 3,040 us, max 12,109,946 us.

| Property | Present | Presence % | Type |
|---|---:|---:|---|
| process | 66,355 | 100.0 | string |
| OSThread | 66,355 | 100.0 | number |
| Func | 66,355 | 100.0 | string |
| txt | 66,354 | 100.0 | string |
| res | 66,320 | 99.9 | string |
| p:processName | 60,817 | 91.7 | string |
| t:clientID | 57,012 | 85.9 | number |
| t:applicationName | 57,012 | 85.9 | string |
| t:computerName | 57,012 | 85.9 | string |
| t:connectID | 10,264 | 15.5 | number |
| SessionID | 10,259 | 15.5 | number |
| Txt | 1 | 0.0 | string |

## EXCPCNTX

Count: 55,104. Collections: Diag, Diag_86, EXP, EXP_86. Avg duration 171,222 us, max 3,600,358,894 us.

| Property | Present | Presence % | Type |
|---|---:|---:|---|
| ClientComputerName | 54,334 | 98.6 | string |
| ServerComputerName | 54,334 | 98.6 | string |
| UserName | 54,334 | 98.6 | string |
| ConnectString | 54,334 | 98.6 | string |
| SrcName | 770 | 1.4 | string |
| process | 770 | 1.4 | string |
| OSThread | 770 | 1.4 | number |
| ClientID | 422 | 0.8 | number |
| p:processName | 371 | 0.7 | string |
| t:clientID | 350 | 0.6 | number |
| CallID | 272 | 0.5 | number |
| MName | 272 | 0.5 | string |
| SessionID | 262 | 0.5 | mixed (num 201, str 61) |
| Usr | 262 | 0.5 | string |
| Context | 244 | 0.4 | string |
| t:applicationName | 204 | 0.4 | string |
| t:computerName | 204 | 0.4 | string |
| t:connectID | 198 | 0.4 | number |
| DBMS | 198 | 0.4 | string |
| DataBase | 198 | 0.4 | string |
| Trans | 198 | 0.4 | number |
| Func | 167 | 0.3 | string |
| Txt | 150 | 0.3 | string |
| callWait | 75 | 0.1 | number |
| first | 75 | 0.1 | number |
| Method | 69 | 0.1 | mixed (num 20, str 49) |
| Module | 49 | 0.1 | string |
| dbpid | 24 | 0.0 | number |
| Sql | 24 | 0.0 | string |
| Form | 22 | 0.0 | string |
| FormItem | 22 | 0.0 | string |
| SearchString | 22 | 0.0 | string |
| IName | 20 | 0.0 | string |
| Rows | 17 | 0.0 | number |
| RowsAffected | 17 | 0.0 | number |
| Interface | 10 | 0.0 | string |
| Sdbl | 1 | 0.0 | string |

## SESN

Count: 36,565. Collections: Diag_86. Avg duration 16,423,299 us, max 4,203,981,094 us.

| Property | Present | Presence % | Type |
|---|---:|---:|---|
| process | 36,565 | 100.0 | string |
| p:processName | 36,565 | 100.0 | string |
| OSThread | 36,565 | 100.0 | number |
| t:clientID | 36,565 | 100.0 | number |
| t:applicationName | 36,565 | 100.0 | string |
| t:computerName | 36,565 | 100.0 | string |
| Func | 36,565 | 100.0 | string |
| IB | 36,565 | 100.0 | string |
| Appl | 36,565 | 100.0 | string |
| Nmb | 36,565 | 100.0 | number |
| ID | 36,565 | 100.0 | string |
| t:connectID | 6 | 0.0 | number |

## QERR

Count: 20,610. Collections: Diag, Diag_86, QERR_Diag. Avg duration 0 us, max 0 us.

| Property | Present | Presence % | Type |
|---|---:|---:|---|
| process | 20,610 | 100.0 | string |
| OSThread | 20,610 | 100.0 | number |
| Usr | 20,610 | 100.0 | string |
| Descr | 20,610 | 100.0 | string |
| Query | 20,610 | 100.0 | string |
| Context | 20,572 | 99.8 | string |
| p:processName | 19,898 | 96.5 | string |
| t:clientID | 19,898 | 96.5 | number |
| t:applicationName | 19,898 | 96.5 | string |
| t:computerName | 19,898 | 96.5 | string |
| t:connectID | 19,898 | 96.5 | number |
| SessionID | 19,898 | 96.5 | number |
| AppID | 5,001 | 24.3 | string |
| DBMS | 8 | 0.0 | string |
| DataBase | 8 | 0.0 | string |

## TLOCK

Count: 10,357. Collections: TLockDiag. Avg duration 2,940,748 us, max 20,017,727 us.

| Property | Present | Presence % | Type |
|---|---:|---:|---|
| process | 10,357 | 100.0 | string |
| p:processName | 10,357 | 100.0 | string |
| OSThread | 10,357 | 100.0 | number |
| t:clientID | 10,357 | 100.0 | number |
| t:applicationName | 10,357 | 100.0 | string |
| t:computerName | 10,357 | 100.0 | string |
| t:connectID | 10,357 | 100.0 | number |
| SessionID | 10,357 | 100.0 | number |
| Usr | 10,357 | 100.0 | string |
| DBMS | 10,357 | 100.0 | string |
| DataBase | 10,357 | 100.0 | string |
| Regions | 10,357 | 100.0 | string |
| Locks | 10,357 | 100.0 | string |
| WaitConnections | 10,357 | 100.0 | mixed (num 10067, str 290) |
| Context | 10,146 | 98.0 | string |
| connectionID | 10,069 | 97.2 | string |
| AppID | 221 | 2.1 | string |
| lka | 1 | 0.0 | number |
| lkaid | 1 | 0.0 | string |
| lkato | 1 | 0.0 | number |

## SCOM

Count: 5,525. Collections: Diag, Diag_86. Avg duration 44,381,618 us, max 17,544,979,244 us.

| Property | Present | Presence % | Type |
|---|---:|---:|---|
| process | 5,525 | 100.0 | string |
| OSThread | 5,525 | 100.0 | number |
| Func | 5,525 | 100.0 | string |
| t:clientID | 5,241 | 94.9 | number |
| t:applicationName | 4,888 | 88.5 | string |
| ProcessName | 1,758 | 31.8 | string |
| SrcProcessName | 1,758 | 31.8 | string |
| p:processName | 101 | 1.8 | string |
| t:computerName | 101 | 1.8 | string |
| t:connectID | 101 | 1.8 | number |
| lka | 2 | 0.0 | number |
| lkaid | 2 | 0.0 | string |
| lkato | 2 | 0.0 | number |
| lkp | 2 | 0.0 | number |
| lkpid | 2 | 0.0 | string |
| lksrc | 2 | 0.0 | string |
| lkpto | 2 | 0.0 | number |

## ADMIN

Count: 1,595. Collections: Diag, Diag_86. Avg duration 0 us, max 0 us.

| Property | Present | Presence % | Type |
|---|---:|---:|---|
| process | 1,595 | 100.0 | string |
| OSThread | 1,595 | 100.0 | number |
| Func | 1,595 | 100.0 | string |
| Administrator | 1,595 | 100.0 | string |
| Result | 1,595 | 100.0 | string |
| p:processName | 1,592 | 99.8 | string |
| t:clientID | 1,592 | 99.8 | number |
| t:applicationName | 1,592 | 99.8 | string |
| t:computerName | 1,592 | 99.8 | string |
| ClusterID | 1,592 | 99.8 | string |
| ConnectionID | 91 | 5.7 | string |
| Mode | 91 | 5.7 | number |
| Ref | 90 | 5.6 | string |
| Host | 90 | 5.6 | string |
| Connection | 90 | 5.6 | number |

## SCALL

Count: 1,182. Collections: CallsDiag, CallsDiag_86. Avg duration 215,118 us, max 2,359,982 us.

| Property | Present | Presence % | Type |
|---|---:|---:|---|
| process | 1,182 | 100.0 | string |
| OSThread | 1,182 | 100.0 | number |
| ClientID | 1,182 | 100.0 | number |
| CallID | 1,182 | 100.0 | number |
| MName | 1,182 | 100.0 | string |
| DstClientID | 1,182 | 100.0 | number |
| Interface | 1,179 | 99.7 | string |
| IName | 1,179 | 99.7 | string |
| Method | 1,179 | 99.7 | number |
| Usr | 560 | 47.4 | string |
| p:processName | 506 | 42.8 | string |
| t:clientID | 432 | 36.5 | number |
| t:applicationName | 432 | 36.5 | string |
| t:computerName | 432 | 36.5 | string |
| t:connectID | 428 | 36.2 | number |
| SessionID | 377 | 31.9 | number |
| AppID | 299 | 25.3 | string |
| Context | 224 | 19.0 | string |
| DBMS | 2 | 0.2 | string |
| DataBase | 2 | 0.2 | string |

## PROC

Count: 722. Collections: Diag, Diag_86. Avg duration 129 us, max 15,976 us.

| Property | Present | Presence % | Type |
|---|---:|---:|---|
| process | 722 | 100.0 | string |
| OSThread | 722 | 100.0 | number |
| Txt | 619 | 85.7 | string |
| Event | 231 | 32.0 | string |
| p:processName | 128 | 17.7 | string |
| t:clientID | 128 | 17.7 | number |
| t:applicationName | 128 | 17.7 | string |
| t:computerName | 128 | 17.7 | string |
| Err | 104 | 14.4 | number |
| t:connectID | 96 | 13.3 | number |
| SessionID | 64 | 8.9 | number |
| Usr | 64 | 8.9 | string |

## TTIMEOUT

Count: 264. Collections: TLockDiag. Avg duration 0 us, max 0 us.

| Property | Present | Presence % | Type |
|---|---:|---:|---|
| process | 264 | 100.0 | string |
| p:processName | 264 | 100.0 | string |
| OSThread | 264 | 100.0 | number |
| t:clientID | 264 | 100.0 | number |
| t:applicationName | 264 | 100.0 | string |
| t:computerName | 264 | 100.0 | string |
| t:connectID | 264 | 100.0 | number |
| SessionID | 264 | 100.0 | number |
| Usr | 264 | 100.0 | string |
| DBMS | 264 | 100.0 | string |
| DataBase | 264 | 100.0 | string |
| WaitConnections | 264 | 100.0 | number |
| Context | 233 | 88.3 | string |
| AppID | 27 | 10.2 | string |

## TDEADLOCK

Count: 32. Collections: TLockDiag. Avg duration 0 us, max 0 us.

| Property | Present | Presence % | Type |
|---|---:|---:|---|
| process | 32 | 100.0 | string |
| p:processName | 32 | 100.0 | string |
| OSThread | 32 | 100.0 | number |
| t:clientID | 32 | 100.0 | number |
| t:applicationName | 32 | 100.0 | string |
| t:computerName | 32 | 100.0 | string |
| t:connectID | 32 | 100.0 | number |
| SessionID | 32 | 100.0 | number |
| Usr | 32 | 100.0 | string |
| DBMS | 32 | 100.0 | string |
| DataBase | 32 | 100.0 | string |
| DeadlockConnectionIntersections | 32 | 100.0 | string |
| Context | 32 | 100.0 | string |

## Anomalies

- Malformed JSON lines found: 0
- Timestamp anomalies (not 202x): 0
- Unusual property names: none
## Time spans and rates

Corpus covers 2025-11-28T21:00 .. 2025-11-30T23:04 (about 50 hours; most collections 2025-11-29..30).

| Collection | First event | Last event | Events | Approx events/sec (observed slice) |
|---|---|---|---:|---:|
| CallsDiag (1 rphost) | 2025-11-30T10:45 | 2025-11-30T14:41 | 120,961 | 8.5 |
| CallsDiag_86 (full) | 2025-11-29T22:00 | 2025-11-30T23:00 | 21,112 | 0.23 |
| Diag (1 rphost) | 2025-11-30T10:00 | 2025-11-30T14:52 | 569,902 | 32.5 |
| Diag_86 (full) | 2025-11-29T22:00 | 2025-11-30T23:01 | 1,256,398 | 13.9 |
| EXP (1 rphost) | 2025-11-29T06:45 | 2025-11-30T23:01 | 79,153 | 0.55 |
| EXP_86 (full) | 2025-11-29T22:00 | 2025-11-30T23:00 | 30,790 | 0.34 |
| Lic (full) | 2025-11-28T23:00 | 2025-11-30T23:01 | 74,382 | 0.43 |
| Lic_86 (full) | 2025-11-29T22:00 | 2025-11-30T22:59 | 3,343 | 0.037 |
| LongDB_01 (1 rphost) | 2025-11-30T16:44 | 2025-11-30T19:51 | 21,198 | 1.9 |
| Mem (1 rphost) | 2025-11-30T20:00 | 2025-11-30T21:53 | 103,959 | 15.3 |
| QERR_Diag (1 rphost) | 2025-11-30T19:11 | 2025-11-30T23:02 | 14,532 | 1.05 |
| TLockDiag (full) | 2025-11-28T23:00 | 2025-11-30T23:02 | 22,018 | 0.13 |
| _AccumRgTn... (1 rphost) | 2025-11-29T09:00 | 2025-11-29T13:42 | 25,287 | 1.5 |
| _Document15317 (full) | 2025-11-29T23:00 | 2025-11-30T23:02 | 79,614 | 0.92 |
| _Reference20832 (1 rphost) | 2025-11-30T19:11 | 2025-11-30T21:43 | 65,675 | 7.2 |
| _Reference27041 (1 rphost) | 2025-11-29T23:00 | 2025-11-30T12:49 | 32,979 | 0.66 |
| _ReferenceChngR12527 (full) | 2025-11-28T21:00 | 2025-11-30T23:03 | 63,768 | 0.35 |

Extrapolated full corpus (per-collection events/MB times full collection size): ~193 million events over the ~50-hour window, i.e. roughly 1,100 events/sec cluster-wide summed over all logcfg filters (events/GB varies 60k/GB for TLockDiag to 4.7M/GB for Diag_86).

## Notes and anomalies

- All 2,585,097 NDJSON lines parsed as valid JSON (json.loads); zero malformed records.
- Every normalizer output .jsonl starts with a UTF-8 BOM (as do the raw .log files); consumers must read with utf-8-sig.
- Giant records: TLOCK events in TLockDiag reach ~3.4 MB per JSON line (Locks property holds full managed-lock dumps with thousands of Fld ranges). 200-line sample of TLockDiag is 42 MB.
- Extreme durations (microseconds): CONN max 17,543,573,004 (about 4.9 h), SCOM max 17,544,979,244, CALL max 5,700,483,937, SESN max 4,203,981,094. Use 64-bit integers.
- Mixed-type properties (same key sometimes numeric, sometimes quoted string): CALL.Method (165,049 num / 16,013 str - string values are method names, numeric are ordinals), CALL.SessionID (8,335 str), CLSTR.SessionID (mostly string - comma-separated lists), CLSTR.SrcPid/DstPid, CONN.lkpid/lksrc, EXCPCNTX.SessionID/Method, DBMSSQL.lkaid, TLOCK.WaitConnections (290 str - comma-separated connection lists). Schema should store these as strings or normalize.
- EXCPCNTX is bimodal: about 99% of records carry only ClientComputerName/ServerComputerName/UserName/ConnectString, about 1% carry the usual process/OSThread/... shape.
- Collections Mem_86, TLockDiag_86, WaitConnections_86 contain only BOM-only (3-byte) .log files - no events at all; normalizer produces empty output for them.
- No DBPOSTGRS, SDBL, DB2, DBORACLE events anywhere in the sampled corpus: the DBMS layer is exclusively DBMSSQL (Microsoft SQL Server). No VRSREQUEST/VRSRESPONSE either.
- All timestamps fall inside 2025-11-28..2025-11-30; none out of range.
- Event names use both upper case (CALL, EXCP) and mixed case (Context); property names include namespaced keys with colons (p:processName, t:clientID, t:connectID, t:applicationName, t:computerName) - column naming must handle ':'.
