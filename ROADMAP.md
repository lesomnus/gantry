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

## 2. 설계 결정 로그 (2026-07-18)

`task260718.md`에서 기존 미결정 질문이 모두 확정됨. 구현 항목은 3절 「retention」에.

- **retention 나이 상한** — 전역 캡은 도입하지 않는다. 대신 keep_n과 무관하게 **M시간
  이상 미사용 이미지를 삭제**하는 하드 idle-age 상한을 둔다(age가 keep_n_recent 보호를
  이긴다). disk pressure를 판단할 수 있으면 그때 나이 순 추가 삭제도 고려(선택).
- **keep-N 카운트 = digest 단위** — 같은 digest를 가리키는 여러 태그는 하나로 센다
  (디스크 회수 granularity와 일치). `Record.Digest`로 그룹핑.
- **pattern pin 미리보기 = 비차단** — 강제/차단형 dry-run은 두지 않는다. `PinService.Add`
  응답에 현재 매칭되는 이미지 refs를 실어 blast radius만 노출한다(기존 `GcPlan` 자발적
  프리뷰는 유지). 차단형 confirm 흐름은 6절(미채택).
- **in-use 주기 heartbeat = 채택** — 저부하이므로 주기적으로 in-use를 스캔해 LastUsed를
  갱신한다(놓친 start 이벤트 보강).

---

## 3. 남은 구현

2절 결정 + 감사 backlog. 성격별.

### retention (2절 결정)
- **하드 idle-age 삭제** — ✅ 완료. 정책에 `max_idle` knob 추가: 평가 순서 in_use/pinned
  다음(keep_n/max_n 이전)에서 idle > max_idle이면 삭제(`idle_exceeded`). disk-pressure 기반
  추가 삭제는 선택 후속(미착수).
- **keep-N/max-N digest 단위 카운트** — ✅ 완료. `digestGroups`로 같은 digest의 태그를 한
  그룹으로 묶어 keep_n/max_n이 digest 기준으로 센다(digest 없는 레코드는 개별 카운트).
- **in-use 주기 heartbeat** — ✅ 완료. `unit.heartbeat` 고루틴이 `retention.heartbeat`(기본
  5m) 주기로 `Engine.InUse`를 읽어 tag/digest ref의 LastUsed=now를 stamp(sha256 image-ID는
  skip). 놓친 start 이벤트를 보강.
- **pattern pin 매칭 echo** — ✅ 완료. `PinService.Add`가 `Manager.PinMatches`로 현재 매칭
  레코드를 계산해 `gantry-pin-matched-count`/`gantry-pin-matched` 트레일러로 노출(비차단;
  refs는 50개 cap, count는 전체). proto 재생성 불요라 트레일러 방식 채택.

### 제품 기능
- **영속 job store** — 접근: `protoc-gen-orm-ent`(protobuf-orm 생태계) + **sqlite3** 백엔드로
  `memStore`를 `Store` seam 뒤에서 교체 → 재시작 시 job history 소실 해소. ⚠️ 정적/scratch
  (CGO=0) 빌드를 유지하려면 **pure-Go 드라이버(modernc.org/sqlite)** 필수 — mattn/go-sqlite3
  (CGO)는 배제.
- **registry-store digest resolve** (낮은 우선순위) — registry의 태그→digest 라이브 조회.
  인벤토리 질의 API가 맞고, 있어서 나쁠 것 없는 nice-to-have.

### 관측성
- **watcher-connected OTel gauge** — 채택. 죽은 usage-event 스트림을 알림으로 잡는다
  (상태는 store RPC로 이미 노출, `Manager.registerGauges`에 추가).
- **GC 결과 카운터**(deleted/untagged/reaped/errors) — 채택. 현재 apply 지점은 slog +
  이벤트 로그만 기록.

### 외부 의존 (mkot, `/workspaces/github.com/lesomnus/mkot`)
- **mkot CI `GOWORK=off` per-module build** + **`pretty` 모듈 테스트 수정**(otx ingress/egress
  렌더링) — mkot main에서 직접 작업, 단계별 커밋(사용자 위임).

### 문서화
- **mutable-tag dedup 주의사항** — ✅ 완료. README "Behavior notes"와 `JobAddRequest` proto
  주석에 dedup key와 mutable-tag 주의사항 명시.

---

## 4. 문서 갭 (v1 감사 결과) — ✅ 해결 (2026-07-17)

문서↔구현 대조에서 나온 문서 오류(4a)와 미문서화(4b)는 문서 전용 패스로 모두 반영했다
(코드 로직 변경 없음; `README.md` · `gantry.yaml` · `docs/test-environment.md` ·
`gantry.nomad.hcl` · `gantry.hday.yaml` 수정). 재확인 완료(2026-07-18). `client_ca`/mTLS는
5a-3에서 **키·dead code 제거**로 결정됨.

---

## 5. v1 릴리즈 체크리스트

### 5a. 블로커 (태그 전 필수)

1. **LICENSE** — 사용자가 직접 추가 예정 → 조치 불요.
2. **bbolt 데이터 호환성** — 릴리즈 전이므로 기존 DB를 신경 쓰지 않음 → 조치 불요.
3. **`serve.auth.client_ca` 제거** — ✅ 완료. 서버측 mTLS API 인증은 미구현 dead code였고,
   bearer + (reverse-proxy) TLS로 충분하므로 config 필드·`auth.go` dead branch·문서를 제거.
   (store-side TPM/`ca_cert` mTLS는 gantry가 client인 정당한 기능이라 유지.)
4. **nomad/hday CLI arg 순서** — 4절에서 수정 완료(재확인함).
5. **proto 버저닝** — 불요. v2가 필요하면 다른 패키지명으로 proto를 만들면 됨(사용자 결정).
6. **버저닝/릴리즈** — 사용자가 직접 → 조치 불요.
7. **CI** — ✅ 완료: `paths`에 `internal/**` 추가, `go test -race ./...` 네이티브 잡 추가
   (이미지 push를 테스트 통과에 게이트).

### 5b. 권장 (should-fix)

- **config strict decode** — ✅ 완료. `yaml.DisallowUnknownField()`로 unknown key를 에러
  처리해 `serve.verify` 오타 등이 보안 컨트롤을 조용히 끄는 것을 막는다.
- **auth-off 경고 로그** — ✅ 완료. auth 미설정 시 기동 로그에 "API authentication is
  DISABLED …" 경고(`rpc.AuthEnabled` 헬퍼로 판정).
- **secrets** — ✅ 완료. store `username`/`password`도 Evaluate에서 `${VAR}` 확장(tokens와
  패리티), `gantry config` 덤프는 `Config.Redacted()`로 토큰·password를 `***`로 마스킹.
- **감사 로그 유실 경고** — ✅ 완료. `Recorder.emit`이 Append 실패를 WARN 로그로 surface하고
  누적 드롭 카운터(`dropped_total`)를 함께 남긴다.
- **deps** — 기능적 문제 없음(빌드/테스트 green, go.sum 고정). 미태깅 pin은 릴리즈 태깅 이슈로
  사용자 소관(5a-6) → 조치 불요.
- **정리** — ✅ 완료. `greet` 서브커맨드/`GreetConfig`/`gantry.yaml` 블록 제거,
  docker-compose 스모크 커맨드를 `serve`로 교체.

---

## 6. 미채택 (재론 금지)

| 제안 | 사유 |
|---|---|
| `GET /metrics` (Prometheus scrape) | OTLP push로 대체 — fleet 파이프라인이 OTel→ClickHouse라 scrape는 이중 파이프라인 |
| `POST /v1/store/{name}/reconcile` (선언형 이미지 셋) | 인벤토리 + 기존 job/remove로 클라이언트가 diff 가능. 서버에 desired-state를 들이면 소유권이 모호 |
| job 완료 webhook/callback | Watch 스트림으로 충분. 엣지에서 아웃바운드 callback은 도달성 문제만 추가 |
| 토큰 관리 API (`/v1/auth/token`) | 정적 토큰 화이트리스트 모델에서 근거 부족. mTLS가 이미 대안 축 |
| retry `force` 플래그 (dedup 우회) | 같은 dst 태그에 writer 둘이 생김. cancel-then-retry가 그 경로 |
| `git_rev` otel resource attribute | 노드 dirty/rebuild 구분용이었으나 불필요 판단 (2026-07-18) |
| pattern-pin 강제/차단 dry-run | 비차단 매칭-echo(2·3절)로 충분 — confirm 흐름은 과함 |
| copy↔downstream insecure 실환경 검증 | 코드가 아니라 수동 E2E 태스크였고 split-brain 한계는 이미 `docs/test-environment.md`에 문서화됨 |
