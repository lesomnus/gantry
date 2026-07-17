# gantry — 로드맵 & 설계 기록

> 이 문서는 기존 `plan.md` / `plan-api.md` / `plan-gc.md` / `plan-digest-as.md` /
> `task.md` 다섯 개를 하나로 통합한 것이다. **구현이 완료된 기능은
> [README](README.md#status)의 Status에 기록**되어 있고, 완료된 설계 문서의 상세
> 원문(전체 rationale·milestone 체크리스트)은 git 히스토리(`aaaf0ef` 및 그 이전
> 커밋)에 그대로 남아 있다. 이 파일은 **앞으로의 것만** 추적한다: 유지할 설계
> 결정, 미결정 질문, 남은 구현, 문서 갭, v1 릴리즈 체크리스트.
>
> 기준: 2026-07-17 문서↔구현 대조 감사(멀티에이전트, 문서 주장 228건 · 구현 기능
> 67개 · plan 항목 27건 전수 검증).

---

## 1. 설계 결정 (유지되는 것)

핵심만 압축. 각 항목의 전체 근거는 git 히스토리의 plan 문서에 있다.

- **store / job / transfer 모델** — 모든 이미지 저장소를 `store`로 통일한다. kind
  `oci`(gantry가 blob read/write)와 `docker`/`containerd`(데몬이 pull). job은
  `{ref, source, target}`로 registry→registry copy 또는 registry→engine pull을
  같은 모양으로 표현하고, 각 이동 단계는 per-layer 진행률을 갖는 `Transfer`로
  보고된다. (원래 warm-centric 설계를 store 모델로 일반화한 결과 — plan.md v2.)
- **seam 구조** — 갈아끼울 수 있는 부분을 `internal/cpx`(registry Source: copy/proxy
  + Warmer 오케스트레이션)과 `internal/down`(engine 드라이버 + capability
  sub-interface)에 격리한다. 새 engine KIND = 파일 하나 + factory case 하나. verify/GC는
  engine이 optional capability를 만족시키면 자동으로 노출되는 additive 확장이다.
- **워밍: copy vs proxy** — `copy`(기본)는 gantry가 upstream에서 pull해 cache로
  push하고 이미 있는 blob은 HEAD로 skip한다 → 실제 이동 byte만 세는 정직한 진행률,
  writable cache 필요. `proxy`는 cache를 EOF까지 read→discard해서 pull-through
  cache가 self-fill하게 유발 → read-only proxy용, 증분 skip 없음.
- **rewrite** — source ref → cache-side ref 매핑은 target store의 순서 있는
  `{glob: template}` 규칙(first-match-wins)으로 산출한다. copy push-dest / proxy
  pull-src / downstream pull ref를 하나의 규칙으로 통일 → API는 항상 canonical
  upstream ref만 받는다.
- **retention: usage-watcher + bbolt 영속 인덱스** — docker도 containerd도 이미지
  last-used를 노출하지 않으므로, gantry가 데몬 컨테이너 이벤트(start/restart/unpause)를
  구독해 그 신호를 직접 만들어 유지한다. 이벤트엔 backlog가 없으므로 인덱스는
  **디스크에 영속**되어야 한다 — gantry가 last-used의 유일한 source of truth다.
  - **왜 bbolt**: CGO_ENABLED=0 / FROM-scratch 정적 바이너리 제약을 만족하는 유일한
    후보. 유일한 transitive dep(`golang.org/x/sys`)은 이미 vendored, 바이너리에 <1MB
    추가. COW + meta-page 2-phase commit이라 엣지 전원 차단에도 인덱스 전체 손상이
    없다. (sqlite는 transpiled libc로 수 MB, 단일 JSON은 event마다 O(N) rewrite,
    in-memory snapshot은 재부팅 시 lastUsed 퇴행 → 잘못된 GC.)
- **정책 cascade + untagged reaper** — 엔진별로 per-repo 규칙을 필드 단위로 cascade
  (longest-prefix 우승, pin은 union). 보호 순서: `in_use` > `pinned` > `keep_n_recent`
  > `within_max_age`, 그 위에 `max_n` 하드 캡. 규칙에 매칭 안 되는 repo는 unmanaged.
  docker 전용 untagged reaper는 태그를 잃고 디스크에 남는 이미지를 관측 시점 시계
  기준 `untagged_after`(기본 1h) 후 삭제한다(containerd는 자체 GC가 처리하므로 이 knob
  거부). GC 패스마다 전체 데몬 인벤토리를 스캔해 out-of-band 변경도 수렴시킨다.
- **verify: notation, admission-time, digest 앵커링, swappable** — job 생성 시점에
  source 서명을 검증하고 실패 시 job을 만들지 않는다(fail-closed). 검증된 digest를
  핀하고 engine pull은 `repo@digest`로 앵커링해 TOCTOU를 제거한다. trust store는
  OS-root fallback 없이 fail-fast(미설정 시 기동 거부). CA 로테이션을 위해 truststore를
  atomic swap으로 핫리로드(실패 시 옛 verifier 유지)한다. proxy 목적지는 digest를
  알 수 없어 검증 경로에서 거부한다.
- **digest-`as`: verbatim 커밋 + 씬 OCI load + containerd-store 감지** — digest로 핀된
  jobspec이 노드에서 **원본 이름 그대로**(`repo@sha256:INDEX`) 로컬 resolve 되도록,
  워밍이 노드 엔진에 digest-named 참조를 만든다. 노드 docker가 containerd image
  store를 쓰면 콘텐츠가 content-addressed라 캐시에서 온 바이트 위에 원본-이름 digest
  참조를 위조할 수 있다 — 씬 OCI 아카이브(`oci-layout` + `index.json` + 앵커 blob
  하나, KB 단위)를 `docker load`(순수 Engine API, tcp 2375만 필요, containerd 소켓
  불요)로 등록한다. classic graph store는 위조가 원천 불가하므로 경고 로그 + skip
  (태그는 정상). hop1 digest-ref 잡은 index를 재조립하지 않고 **verbatim 커밋**해
  소스 digest를 보존한다.
- **job store는 in-memory** — job record는 `JobTTL` 후 evict되는 휘발성. 캐시 채우기
  용도에선 재시작 시 job history 소실이 수용 가능하고, `Store` interface seam으로
  두어 향후 bbolt/sqlite drop-in이 가능하다. 재시작 내구 이력이 필요한 감사 용도는
  별개의 bbolt 이벤트 로그(`serve.events`)가 담당한다.
- **관측: OTLP push (Prometheus scrape 미채택)** — fleet 관측 파이프라인이 이미 노드
  로컬 OTel collector → ClickHouse/Grafana이므로 scrape 엔드포인트는 이중 파이프라인이
  된다. 메트릭/트레이스/로그는 mkot OTLP push로 통일한다(exporter 미설정 = no-op이
  의도된 기본값).

---

## 2. 미결정 질문 (설계 확정 필요)

구현 전에 방향을 정해야 하는 것들. 현재 동작은 잠정 결정 상태.

- **keep_n 범위** — 현재 per-repo N + per-repo `max_n` 캡. repo가 아주 많은 엣지에서
  총 보존량이 커질 수 있는데 전역/엔진별 캡을 추가할지. (audit: `max_n`은 있으나
  글로벌 캡 없음 → 총량 무제한.)
- **keep-N: 태그 vs digest 카운트** — 여러 태그가 같은 digest를 가리킬 때 keep-N을
  태그 단위로 셀지 digest 단위로 셀지. 디스크 회수는 digest 단위(마지막 태그 제거
  시)지만 사용자 의도는 태그 단위 — 현재 태그 단위. (digest-as 작업이 `Record.Digest`를
  추가했으나 그룹핑엔 아직 미사용.)
- **pattern pin 강제 dry-run** — 부주의한 광범위 패턴(`*`)이 GC를 무력화할 수 있다.
  현재는 자발적 `GcPlan` 프리뷰만 가능하고 pin 생성 시 강제 확인 흐름은 없다.
- **in-use → LastUsed heartbeat reconciler** — start 이벤트를 놓친 채 돌고 있는
  컨테이너를 위해 주기적 in-use 스캔으로 LastUsed를 찍을지. 현재는 watcher 재접속
  재시드 + 매 GC 패스 in-use 보호로 완화하고 있어 정상 상황에선 불필요.

---

## 3. 남은 구현

audit가 `missing`/`partial`로 판정한 항목. 우선순위·성격별로 분류.

### 제품 기능
- **영속 job store** — `memStore`가 in-memory only라 재시작 시 job history 소실.
  `Store` interface seam은 준비되어 있음(bbolt/sqlite drop-in). 의도된 phase-2
  연기이며 감사 필요는 이벤트 로그가 대체하므로, **v1에는 "재시작 시 job history는
  소실(영속 기록은 이벤트 로그)"을 문서화하는 것으로 충분**하고 구현은 v1.1 후보.
- **registry-store digest resolve** — "캐시에 이미지 X가 digest Y로 있나"를 라이브
  remote resolve로 답하는 인벤토리 질의. 현재 인벤토리는 engine store만 bbolt 인덱스
  기반. bbolt의 믿음과 라이브 remote lookup을 한 경로에 섞지 않으려 의도적으로 분리됨.

### 관측성
- **watcher-connected OTel gauge** — usage-watcher 상태 플래그를 게이지로 push해
  죽은 이벤트 스트림을 알림으로 잡는다. 상태 자체는 store RPC로 이미 노출되고 게이지만
  없음(`Manager.registerGauges`에 추가 지점 존재).
- **GC 결과 카운터** — deleted/untagged/reaped/errors를 메트릭으로. 현재 apply 지점은
  slog + 이벤트 로그만 기록하고, retention 메트릭은 인벤토리 카운트 게이지뿐.
- **git_rev otel resource attribute** — `resource/gantry` processor가 service.name /
  service.version만 설정. dirty/rebuild 노드 구분용 GitRev/GitDirty 미추가
  (`version.Get()`은 이미 import됨).

### 외부 의존 (mkot 리포)
- **mkot CI `GOWORK=off` per-module build** — 중첩 모듈 version-skew 재발 방지.
  mkot 리포엔 아직 CI가 전무.
- **mkot `pretty` 모듈 테스트 실패** — otx ingress/egress 렌더링 WIP. gantry 빌드·테스트엔
  무영향, 의도적 out-of-scope.

### 문서화·검증 태스크
- **mutable-tag dedup 주의사항 문서화** — dedup은 active job 동안 태그를 stable로
  취급하므로, mid-job에 재push된 태그는 첫 job이 끝날 때까지 두 번째 워밍을 안 띄운다.
  README/proto 주석 어디에도 명시 안 됨.
- **copy↔downstream insecure split-brain 검증** — 동작·에러 표면·pull_host 워크어라운드는
  구현·문서화됨. loopback(127.0.0.0/8) 밖 실환경 검증만 남음.

---

## 4. 문서 갭 (v1 감사 결과) — ✅ 해결 (2026-07-17)

문서↔구현 대조에서 나온 문서 오류(4a)와 미문서화(4b)는 문서 전용 패스로 모두 반영했다
(코드 로직 변경 없음; `README.md` · `gantry.yaml` · `docs/test-environment.md` ·
`gantry.nomad.hcl` · `gantry.hday.yaml` 수정). 유일한 잔여는 `client_ca`/mTLS로, 문서는
"미작동"으로 정직하게 고쳤지만 구현할지 키·코드를 제거할지는 아래 5a-3 결정 사항이다.

---

## 5. v1 릴리즈 체크리스트

### 5a. 블로커 (태그 전 필수)

1. **LICENSE 없음** — 공개 모듈(`github.com/lesomnus/gantry`)인데 라이선스 파일 전무 →
   법적으로 all-rights-reserved, pkg.go.dev 미표시. 최우선.
2. **bbolt 데이터 호환성 파괴** (uncommitted diff) — retention 인덱스의 영속 JSON 키가
   리네임됨(`last_used`→`date_last_used`, `pinned_at`→`date_pinned`, `first_seen`→
   `date_first_seen` 등). 기존 DB 레코드가 zero 타임스탬프로 디코드되어 업그레이드 직후
   수동 `GcApply`가 오판할 수 있다. **구 키 폴백 디코드를 넣거나 JSON 키 리네임만 되돌릴
   것** (아직 커밋 전 — 지금이 가장 쌈; hday 랩에 이미 돌고 있는 노드 존재).
3. **`client_ca` 죽은 mTLS** (4a와 동일) — 구현하거나 키·문서 제거. 보안 표면이라 중요.
4. **nomad/hday CLI arg 순서** (4a와 동일) — 배포 예시가 실행 불가.
5. **proto 패키지 미버저닝** — 전 파일이 `package gantry`(v1 네임스페이스 없음). 며칠 전까지
   breaking 변경(9bc76ff, aaaf0ef)이 들어온 계약이므로 `gantry.v1`로 갈지 태그 전에 결정.
   태그 후엔 escape hatch가 없다.
6. **버저닝/릴리즈** — git 태그 0개, CHANGELOG 없음, semver 개념 없음(날짜 스킴 `YYMMDD-rN`).
   생성 파일 `cmd/version/version.g.go`가 stale 값으로 **커밋돼 있어** `go install` 사용자가
   그 스탬프를 받음 → gitignore 처리 또는 clean 값. v1.0.0 태그 + 릴리즈 노트 필요.
7. **CI 커버리지** — `.github/workflows/ci.yaml`의 `paths:` 필터에 `internal/**` 누락
   (현재 uncommitted diff 전체가 internal-only → 이런 PR은 CI 미트리거). 테스트도 `-race`
   없이 데몬 없는 이미지 빌드 안에서 돌아 통합 테스트가 전부 조용히 skip.

### 5b. 권장 (should-fix)

- **config fail-open** — unknown key 무시(strict 디코드 아님) → `serve.verify` 오타가
  서명 검증을 조용히 끈다. goccy는 strict 모드를 지원하니 켤 것.
- **auth-off 기본값 경고** — `:8080` 전 인터페이스 바인드 + auth 미설정 시 비활성인데 경고
  로그가 없다. 포트에 닿는 누구나 파괴적 RPC(`Remove`/`GcApply`/unpin) 호출 가능 → 최소한
  기동 시 경고.
- **secrets 스토리** — store password는 env 확장이 안 됨(`tokens`만 됨), `*_file` 간접
  지정도 없음. `gantry config`는 시크릿 포함 전체 설정을 평문 덤프.
- **감사 로그 유실 무음** — `recorder.go`가 Append 에러를 버림 → 디스크 풀 등 정확히 감사가
  필요한 순간에 기록이 조용히 멈춤. slog 경고 + 드롭 카운터.
- **deps** — 미태깅 pseudo-version 8개(lesomnus/{mkot,otx,xli,z} 계열 + protobuf-orm +
  google.golang.org/protobuf pre-release 커밋). 자체 라이브러리 태깅 + protobuf 정식 버전.
- **정리** — `greet` 서브커맨드/설정 블록(스캐폴드), 한국어 스크래치 `task.md`(→ 본 통합으로
  삭제).

---

## 6. 미채택 (재론 금지)

| 제안 | 사유 |
|---|---|
| `GET /metrics` (Prometheus scrape) | OTLP push로 대체 — fleet 파이프라인이 OTel→ClickHouse라 scrape는 이중 파이프라인 |
| `POST /v1/store/{name}/reconcile` (선언형 이미지 셋) | 인벤토리 + 기존 job/remove로 클라이언트가 diff 가능. 서버에 desired-state를 들이면 소유권이 모호 |
| job 완료 webhook/callback | Watch 스트림으로 충분. 엣지에서 아웃바운드 callback은 도달성 문제만 추가 |
| 토큰 관리 API (`/v1/auth/token`) | 정적 토큰 화이트리스트 모델에서 근거 부족. mTLS가 이미 대안 축 |
| retry `force` 플래그 (dedup 우회) | 같은 dst 태그에 writer 둘이 생김. cancel-then-retry가 그 경로 |
