# sqlitem

터미널에서 SQLite 데이터베이스를 다루는 단일 바이너리 TUI 도구입니다. 스키마를 항상 옆에 둔 채로 SQL을 작성·실행하고, 결과 그리드에서 데이터를 바로 고칠 수 있습니다.

SSH로 접속한 리눅스/맥 서버에서 SQLite 파일을 확인하거나 수정해야 하는 개발자와 운영자를 위해 만들었습니다.

## 주요 기능

- **스키마 트리를 항상 표시**: 왼쪽 패널에 테이블·뷰와 컬럼(타입, PK/NN)이 늘 보입니다.
- **vi 스타일 SQL 편집기**: 여러 줄 작성, 문법 색상, 되돌리기, SQL 포맷팅을 지원합니다.
- **결과 그리드에서 바로 편집**: 셀 수정, 행 삽입, 행 삭제를 그리드에서 할 수 있습니다. PK가 없는 테이블도 rowid로 정확히 한 행만 바뀝니다.
- **안전장치**: UPDATE, DELETE, DROP은 반드시 확인을 거칩니다. 모든 쓰기 작업은 변경 전후 값과 함께 로그로 남습니다.
- **어디서나 실행**: 설치 없이 바이너리 하나만 복사하면 됩니다. CGO나 libsqlite가 필요 없습니다.
- **느린 링크 대응**: 바뀐 줄만 다시 그려서 느린 SSH 연결에서도 가볍게 동작합니다.
- **터미널 기본 동작 유지**: 마우스를 가로채지 않으므로 드래그 선택과 우클릭 복사가 평소처럼 됩니다.
- **TTY 없는 모드**: 파이프·스크립트용 REPL, 단발 실행, ASCII 출력 모드를 제공합니다.

## 설치

`build/` 디렉토리에 바이너리가 만들어집니다.

```sh
make build               # 현재 OS/아키텍처용 → build/sqlitem
make build-linux-amd64   # 리눅스 x86-64용   → build/sqlitem-linux-amd64
make build-linux-arm64   # 리눅스 ARM64용    → build/sqlitem-linux-arm64
make build-linux         # 리눅스 두 가지 모두
```

모든 바이너리는 정적 링크입니다. 서버에는 파일 하나만 복사하면 됩니다.

```sh
scp build/sqlitem-linux-amd64 server:~/bin/sqlitem
```

버전을 넣으려면 `VERSION=1.0.0 make build`처럼 실행합니다. 그 밖에 `make test`, `make vet`, `make clean`을 쓸 수 있습니다.

## 빠른 시작

```sh
sqlitem app.db
```

1. 시작하면 **Schema** 패널에 포커스가 있습니다. `j`/`k`로 테이블을 고르고 `Enter`를 누릅니다.
2. 편집기에 `SELECT * FROM <table> LIMIT 100;`이 채워집니다. `F5`를 눌러 실행합니다.
3. 결과가 **Result** 그리드에 표시됩니다. `h j k l`로 셀을 옮겨 다니고, `e`로 셀을 수정합니다.
4. 종료는 `:q` 후 `Enter`, 또는 `Ctrl+Q`입니다.

## 실행 모드

DB 파일 경로는 필수입니다. 첫 번째 인자나 `-db` 플래그로 넘깁니다. 파일이 없으면 새로 만듭니다.

| 명령 | 설명 |
|---|---|
| `sqlitem app.db`, `sqlitem -db app.db` | 전체 화면 TUI |
| `sqlitem -console app.db` | 라인 기반 REPL. TTY가 필요 없어 파이프나 스크립트에 쓸 수 있습니다. |
| `sqlitem -ascii app.db` | ASCII 테이블로 출력하는 REPL. Unicode가 깨지는 터미널용입니다. |
| `sqlitem -exec "<sql>" app.db` | SQL을 실행하고 종료합니다. |
| `sqlitem -query "<sql>" app.db` | 읽기 전용 쿼리를 실행하고 종료합니다. 쓰기 문장은 거부합니다. |

| 플래그 | 설명 |
|---|---|
| `-yes` | 비대화형 모드에서 UPDATE/DELETE/DROP을 확인 없이 실행합니다. |
| `-log <path>` | 변경 로그 파일 경로를 지정합니다. |
| `-ascii` | `-exec`/`-query` 결과도 ASCII 테이블로 출력합니다. |
| `-version` | 버전을 출력합니다. |

입력이나 출력이 터미널이 아니면(파이프 등) TUI 대신 자동으로 콘솔 REPL로 실행됩니다.

## 화면 구성

```
╭─* Schema (2) ──────╮╭─  SQL [INSERT] 1:32 ─────────────────────────╮
│▾ orders            ││ 1 SELECT * FROM users LIMIT 100;             │
│  ├ id    integer PK││ ~                                            │
│  ├ user_id      int│╰──────────────────────────────────────────────╯
│  └ memo        text│╭─  Result 1/3 [users: editable] ──────────────╮
│▾ users             ││#│ id │ name   │ email           │ age  │      │
│  ├ id    integer PK││─┼────┼────────┼─────────────────┼──────┼      │
│  ├ name     text NN││1│  1 │ 김철수 │ kim@example.com │   30 │      │
│  └ email       text││2│  2 │ Lee    │ lee@x.io        │ NULL │      │
╰────────────────────╯╰──────────────────────────────────────────────╯
3 rows, 1ms                                               ← 상태바
```

- **Schema (왼쪽)**: 테이블·뷰와 컬럼 트리
- **SQL (오른쪽 위)**: SQL 편집기. 제목에 현재 모드와 커서 위치가 표시됩니다.
- **Result (오른쪽 아래)**: 결과 그리드
  - 제목에 현재 행 위치와 편집 가능 여부가 표시됩니다. 예: `[users: editable]`, `[read-only: JOIN query]`
  - 맨 앞 `#` 컬럼은 1부터 시작하는 일련번호입니다.
- **상태바 (맨 아래)**: 실행 결과, 오류, 안내 메시지

포커스된 패널은 테두리와 제목이 밝게 강조되고, 제목 앞에 `*`가 붙습니다. `Tab` / `Shift+Tab`으로 패널을 옮깁니다.

## 사용법

### 스키마 둘러보기

- **트리 표시**: 테이블이 10개 미만이면 처음부터 모두 펼쳐져 있고, 10개 이상이면 접혀 있습니다.
- **이동**: `j`/`k`로 위아래로 움직입니다. 펼친 컬럼까지 이어서 이동하므로, 테이블의 마지막 컬럼에서 `j`를 누르면 다음 테이블로 넘어갑니다.
- **펼치기와 접기**: `l`은 펼치기 또는 첫 컬럼으로 진입, `h`는 소속 테이블로 이동 또는 접기입니다.
- **검색**: `/`를 누르고 이름을 입력하면 테이블명이나 컬럼명이 일치하는 테이블만 남습니다. `Enter`는 필터를 유지하고, `Esc`는 해제합니다.
- **DDL 보기**: `v`는 선택한 테이블의 DDL을 인덱스·트리거와 함께 보여 주고, `V`는 전체 스키마 DDL을 보여 줍니다.
- **이름 복사**: `y`를 누르면 테이블명이나 컬럼명이 클립보드에 복사됩니다.
- **자동 갱신**: 편집기에서 CREATE/DROP/ALTER를 실행하면 트리가 바로 갱신됩니다. `r`로 직접 다시 읽을 수도 있습니다.

### SQL 작성과 실행

편집기는 **INSERT 모드**(일반 타이핑)로 시작합니다. `Esc`를 누르면 **NORMAL 모드**로 바뀌고, vi 명령을 쓸 수 있습니다.

```sql
SELECT * FROM users WHERE age > 20;
UPDATE users SET age = age + 1 WHERE id = 3;   -- 커서를 여기 두고 F3
SELECT count(*) FROM orders;
```

- **전체 실행**: `F5` 또는 `Ctrl+R`. 여러 문장을 `;`로 구분해 차례로 실행합니다.
- **커서 문장 실행**: `F3` 또는 `Ctrl+K`. 커서가 있는 문장 하나만 실행합니다. VISUAL 모드에서는 선택한 부분을 실행합니다.
- **포맷팅**: `Ctrl+F`를 누르면 키워드를 대문자로 바꾸고 절마다 줄을 나눕니다.
- **실행 취소**: 오래 걸리는 쿼리는 `Ctrl+C`로 중단합니다.
- **결과 표시**: 마지막으로 행을 반환한 문장의 결과가 그리드에 표시되고, 상태바에 행 수와 소요 시간이 나옵니다.

### 결과 보기

- **이동**: `h j k l`로 셀 단위 이동, `Ctrl+F`/`Ctrl+B`로 20행 단위 이동, `gg`/`G`로 첫 행과 마지막 행으로 갑니다.
- **값 표시**: 긴 값은 잘려 보이고, 줄바꿈은 `↵`로 표시됩니다. `Enter`를 누르면 셀 전체 값을 팝업으로 볼 수 있습니다.
- **복사**: `y`로 현재 셀을 복사합니다. `v`로 VISUAL 블록(행×열)을 지정한 뒤 `y`를 누르면 블록이 TSV로 복사되어 스프레드시트에 바로 붙여 넣을 수 있습니다.
- **원격 복사**: SSH 환경에서도 OSC 52를 지원하는 터미널(iTerm2, WezTerm, kitty, tmux 등)이면 로컬 클립보드로 복사됩니다.

### 데이터 편집

**단일 테이블 SELECT 결과**에서만 편집할 수 있습니다. 그리드 제목에 `[테이블명: editable]`이 보이면 편집 가능한 상태입니다.

**셀 수정** (`e` 또는 `u`)
1. 편집 창에서 값을 입력합니다. NULL을 넣으려면 `Ctrl+N`으로 **Set NULL**을 체크합니다.
2. `Enter`를 누르면 **변경 전/후 값**을 보여 주는 확인 창이 뜹니다.
3. **Apply**를 누르면 적용되고, **Back**을 누르면 입력하던 편집 창으로 돌아갑니다.

**행 삽입** (`i`)
- 컬럼별로 값을 입력합니다. 손대지 않은 컬럼은 DEFAULT 값이 들어갑니다.
- `Ctrl+N`은 NULL, `Ctrl+D`는 DEFAULT로 되돌립니다.
- 확인 창을 거친 뒤 삽입됩니다.
- 결과가 0행이어도 `i`로 행을 추가할 수 있습니다.

**행 삭제** (`d`)
- 현재 행을 지웁니다. VISUAL 블록을 지정했다면 블록에 포함된 행들을 모두 지웁니다.
- **삭제 확인 → 재확인**의 두 단계를 거칩니다. 재확인에서 Back을 누르면 첫 확인 창으로 돌아갑니다.

변경 결과는 그리드와 상태바에 바로 반영됩니다.

**편집할 수 없는 경우**

아래 경우에는 편집 키가 무시되고 상태바에 이유가 표시됩니다.

| 경우 | 예 |
|---|---|
| 여러 테이블 | JOIN, `FROM a, b` |
| 집계 | `count(*)`, `GROUP BY` |
| 그 밖의 쿼리 형태 | DISTINCT, UNION, FROM 절의 서브쿼리 |
| 편집할 수 없는 대상 | 뷰, WITHOUT ROWID 테이블 |
| 읽기 전용 컬럼 (헤더가 흐리게 표시됨) | `age + 1` 같은 계산 컬럼, 생성 컬럼, rowid 자체인 `INTEGER PRIMARY KEY` 컬럼 |

### 안전장치와 변경 로그

**확인 절차**
- 편집기에서 실행한 UPDATE, DELETE, DROP은 확인 창을 거칩니다.
- 확인 창에는 실행할 문장과 **영향받을 행 수**가 표시됩니다.
- 확인 창의 기본 선택은 Cancel이라, 실수로 `Enter`를 눌러도 실행되지 않습니다.

**변경 로그**
- 모든 쓰기 작업은 JSON Lines 파일에 한 줄씩 기록됩니다. 그리드 편집과 편집기에서 직접 실행한 SQL이 모두 포함됩니다.
- 기본 경로는 `<DB 파일>.sqlitem.log.jsonl`입니다. 그 위치에 쓸 수 없으면 `~/.sqlitem/`에 저장합니다. 경로는 `-log` 플래그로 바꿀 수 있습니다.

```json
{"time":"2026-10-02T08:40:38+09:00","db":"/data/app.db","op":"update","table":"users","rowid":2,
 "sql":"UPDATE \"users\" SET \"name\" = ? WHERE rowid = ?","args":["Yi",2],"rows_affected":1,
 "before":[{"rowid":2,"id":2,"name":"Lee","email":"lee@x.io","age":null}],
 "after":[{"rowid":2,"id":2,"name":"Yi","email":"lee@x.io","age":null}]}
```

| 작업 | 기록 내용 |
|---|---|
| UPDATE | 변경 전(before)과 변경 후(after) 행 |
| DELETE | 삭제된 행(before) |
| INSERT | 삽입된 행(after) |
| DDL | 실행한 SQL |

before/after 행은 최대 1000행까지 캡처합니다.

### 콘솔 · 스크립트에서 사용

```sh
# 대화형 REPL (';'로 끝나면 실행)
sqlitem -console app.db

# 파이프로 SQL 실행
echo "SELECT name, age FROM users WHERE age > 20;" | sqlitem -console app.db
sqlitem -console app.db < migrate.sql

# 단발 실행
sqlitem -query "SELECT count(*) FROM users" app.db
sqlitem -exec  "INSERT INTO users(name) VALUES ('kim')" app.db

# 확인이 필요한 문장을 자동화할 때
sqlitem -yes -exec "DELETE FROM sessions WHERE expired = 1" app.db
```

**비대화형 모드의 확인**
- UPDATE/DELETE/DROP은 터미널로 `y/N`을 물어봅니다.
- 터미널이 없으면(cron 등) 실행하지 않고 실패로 끝납니다. 이때는 `-yes`를 명시해야 실행됩니다.
- 오류가 나면 종료 코드 1을 반환합니다.

**REPL 명령**

| 명령 | 설명 |
|---|---|
| `.tables` | 테이블·뷰 목록 |
| `.schema [이름]` | 전체 DDL, 또는 지정한 테이블의 DDL(인덱스·트리거 포함) |
| `.columns <테이블>` | 컬럼 목록 (타입, PK/NN) |
| `.log` | 변경 로그 파일 경로 |
| `.quit` | 종료 |

## 키 바인딩

### 전역

| 키 | 동작 |
|---|---|
| `Tab` / `Shift+Tab` | 다음 / 이전 패널 (`Ctrl+Tab` / `Ctrl+Shift+Tab`은 터미널이 보내 주는 경우에만 동작) |
| `F5`, `Ctrl+R` | 편집기 전체 실행 |
| `F3`, `Ctrl+K` | 커서가 있는 문장(또는 VISUAL 선택 영역) 실행 |
| `Ctrl+F` | SQL 포맷팅 (Result 패널에서는 20행 아래로 이동) |
| `Ctrl+C` | 실행 중인 쿼리 취소, 실행 중이 아니면 종료 |
| `:q`, `Ctrl+Q` | 종료 (`:`는 편집기 INSERT 모드와 검색 입력 중에는 글자로 입력됨) |
| `F1`, `?` | 도움말 |

### Schema

| 키 | 동작 |
|---|---|
| `j` / `k`, `↓` / `↑` | 아래 / 위로 이동 |
| `l`, `→` | 펼치기, 이미 펼쳐져 있으면 첫 컬럼으로 이동 |
| `h`, `←` | 컬럼이면 소속 테이블로 이동, 테이블이면 접기 |
| `gg`, `Home` / `G`, `End` | 맨 위 / 맨 아래 |
| `Enter` | 스타터 쿼리를 편집기에 채우고 편집기로 이동 |
| `y` | 테이블명 / 컬럼명 복사 |
| `v` / `V` | 선택 테이블 DDL / 전체 DDL 보기 |
| `/` | 검색 (`Enter` 유지, `Esc` 해제) |
| `r` | 스키마 다시 읽기 |

### SQL 편집기

| 모드 | 키 | 동작 |
|---|---|---|
| INSERT | 일반 입력 | 텍스트 입력 (`Enter`를 누르면 들여쓰기를 유지한 채 줄바꿈) |
| INSERT | `Esc` | NORMAL 모드로 전환 |
| NORMAL | `i a I A o O` | INSERT 모드로 전환 |
| NORMAL | `h j k l w b e 0 ^ $ gg G` | 커서 이동 |
| NORMAL | `x` `dd` `D` `C` `cc` `J` | 삭제 / 변경 / 줄 합치기 |
| NORMAL | `yy` `p` `P` | 줄 복사 / 붙여넣기 |
| NORMAL | `u` | 되돌리기 |
| NORMAL | `v` / `V` | VISUAL 문자 / 라인 선택 |
| VISUAL | `y` `d` `c` | 선택 영역 복사 / 삭제 / 변경 |
| VISUAL | `Esc` | 선택 해제 |

### Result

| 키 | 동작 |
|---|---|
| `h j k l`, 방향키 | 셀 단위 이동 |
| `Ctrl+F` / `Ctrl+B` | 20행 아래 / 위 (`PgDn` / `PgUp`도 동작) |
| `gg`, `Home` / `G`, `End` | 첫 행 / 마지막 행 |
| `0` / `$` | 첫 컬럼 / 마지막 컬럼 |
| `v` | VISUAL 블록 지정 / 해제 |
| `y` | 셀 복사 (VISUAL이면 블록 복사) |
| `e`, `u` | 셀 편집 (VISUAL 중에는 무효) |
| `i` | 행 삽입 |
| `d` | 행 삭제 (VISUAL이면 선택 행 전체) |
| `Esc` | VISUAL 해제 |
| `Enter` | 셀 전체 값 보기 (`q`, `Enter`, `Esc`로 닫기) |

### 모달 창

| 창 | 키 | 동작 |
|---|---|---|
| 확인 창 | `y` / `n` | 실행 / 취소 |
| 확인 창 | `←` `→` + `Enter` | 버튼 선택 후 실행 |
| 편집 · 삽입 창 | `Tab` / `Shift+Tab` | 필드 이동 |
| 편집 · 삽입 창 | `Ctrl+N` | NULL 지정 |
| 편집 · 삽입 창 | `Enter` | 제출 |
| 편집 · 삽입 창 | `Esc` | 취소 |
| 텍스트 보기 (DDL, 셀 값, 도움말) | `j` / `k` | 스크롤 |
| 텍스트 보기 | `y` | 내용 복사 |
| 텍스트 보기 | `q`, `Enter`, `Esc` | 닫기 |

## 참고

- 결과는 최대 100,000행까지 메모리에 읽습니다. 넘으면 그리드 제목에 `+`가 붙습니다. 큰 테이블은 `LIMIT`을 사용하세요.
- 사용자가 직접 `BEGIN`을 실행해 트랜잭션이 열려 있는 동안에는 그리드 편집(자체 트랜잭션 사용)이 실패할 수 있습니다. `COMMIT`이나 `ROLLBACK` 후에 편집하세요.
- 요구사항 문서는 [PRD.md](PRD.md)에 있습니다.
