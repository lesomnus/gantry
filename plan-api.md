# gantry phase-3 설계 — API 확장 로드맵

2026-07-02. 현 API 표면(swagger)과 internal 코드를 대조해 도출한 확장 목록.
운영자(3am 디버깅) / 자동화(CI·fleet reconciler) / 관측성 / 공급망 보안 네 관점에서 제안을 만들고,
각 항목을 실제 코드에 대조 검증해 확정한 것만 남겼다. 착수 순서대로 정리.

원칙:

- 기존 스타일 유지 — `/v1` prefix, store-scoped 경로, `writeJSON`/`writeErr`, 422=검증 실패, 501=capability 없음.
- 새 엔드포인트마다 swag 어노테이션 → `internal/server/oapi` 재생성(`scripts/fix-openapi.py`, `gen-api-docs.sh`) 동반.
- **Prometheus `/metrics` scrape 엔드포인트는 채택하지 않는다.** 메트릭은 mkot OTLP push로 통일(§2.6).

---

## 0단계 — 선행 수정 (API 추가 없음, 뒤 단계가 의존)

전부 작고 독립적. 이후 단계의 정합성이 여기 걸려 있어 먼저 처리한다.

1. **job sweeper 배선** — `memStore.Sweep`(internal/warm/store.go:103)과 `Warm.JobTTL`(기본 30m)이
   존재하지만 **프로덕션 호출자가 없다**. gantry.yaml의 `job_ttl` 주석은 없는 동작을 설명 중이고,
   완료 job이 메모리에 무한 누적된다. `Warmer.Start`(warm.go:127)에 ticker goroutine
   (`min(ttl/4, 1m)`) 추가, base ctx로 종료.
2. **실패를 canceled로 둔갑시키는 버그** — layer copy 실패 시 `once.Do + job.Cancel()`
   (warm.go:405-417) 경로에서 `finish()`가 ctx.Err()를 먼저 봐서 JobFailed 대신
   JobCanceled로 기록된다. `finish()`(warm.go:473-489)에서 firstErr를 ctx.Err()보다 먼저 확인.
3. **`down.Engine.Remove`의 NotFound 관용** — docker(docker.go:140)/containerd(containerd.go:183)의
   Remove가 raw error를 그대로 올려서, 밖에서(`docker rmi`) 지워진 이미지의 retention 레코드는
   매 GC apply마다 영원히 에러난다. NotFound는 성공으로 취급 → GC가 고아 레코드를 self-heal.
4. **`Decision.earliestAgeOut` 직렬화** — unexported(internal/retention/record.go:63)라
   dry-run 응답에 next_age_out이 안 나간다. export 또는 MarshalJSON.
5. **수동 pull의 index stamp** — `POST /v1/store/{name}/pull`(handlers_store.go:60-69)이
   retention index를 안 건드려서, 이 경로로 받은 이미지는 Record가 없어 **age-GC 영구 불가**
   (Evaluate는 index 레코드만 순회). 성공 시 `Index.Distributed(name, ref, now)` 호출 —
   job 파이프라인의 distHook(cmd/serve.go:71) 및 `/remove`의 index 동기화(handlers_gc.go:48-50)와 동형.

---

## 1단계 — 검증 체인 완결 (보안, 최우선)

notation 검증이 job 어드미션에만 있고 그 뒤가 끊겨 있다. kamino(코어에서 검증·재서명 →
엣지에서 재검증) 설계가 성립하려면 이 세 개가 먼저다.

### 1.1 distribute digest 앵커링 — TOCTOU 제거

`plan()`이 검증된 digest를 `ex.src`에 핀하지만(warm.go:239), 엔진 pull은 캐시 **태그**로 나간다
(pullBase = ex.dst, warm.go:250-253). 캐시 repo가 zot sync prefix 아래면 zot이 태그를 upstream에
재해석하므로: gantry가 digest D를 검증·push한 뒤 태그가 미서명 D'로 이동했으면 **전 노드가
미검증 바이트를 실행**하고 gantry는 transfer done + retention stamp까지 찍는다.
plan.md(:244, :589)는 원래 "index digest로 distribute"를 약속했는데 구현이 태그로 내려앉은 것.

- 검증된(또는 copy-commit된) manifest/index digest를 각 engineStep에 넘겨 `repo@digest`로 pull
  (또는 태그 pull 후 로컬 resolve digest 불일치 시 transfer failed).
- `TransferSnapshot`에 expected/pulled digest 노출.
- effort: medium. 1.2와 같은 코드 영역이라 묶어서 진행.

### 1.2 `POST /v1/job`에 서명/referrer 전파 (`copy_referrers`)

`copySource.Commit`(source_copy.go:56-102)이 platform-filter된 index를 **새로 만들어** push하므로
referrer(notation 서명)가 캐시에서 탈락한다 — 어드미션에서 검증한 바로 그 서명이 첫 hop에서
사라져, 캐시를 `from`으로 삼는 엣지 gantry는 require 모드에서 전부 ErrUnsigned.

- `copy_referrers` 플래그(verifier 활성 시 기본 on): 소스의 OCI referrer를 열거해 캐시에 함께 push.
- **digest 보존이 전제**: referrer copy 시 rebuilt index 대신 원본 index verbatim push
  (`remote.Put`에 raw descriptor; 자식 manifest 먼저 warm) — platforms 필터와는 400 또는 무시로 정리.
- 구현: ggcr `remote.Referrers` + per-referrer `remote.Get/Put`, 또는 oras-go v2 `oras.CopyGraph`
  (referrers-API vs fallback-tag를 알아서 처리, zot에 유리 — verify가 이미 oras repo를 씀,
  notation.go:112-127).
- effort: medium.

### 1.3 패턴 pin 구현 — `/v1/store/{name}/pin`

plan-gc.md §1·§5가 패턴 pin(`prod-*`, `*:latest`, 예시 body `"pins": ["*:stable"]`)을 문서화했지만
`Evaluate`는 **정확 일치 map lookup만** 한다(policy.go:17-27). 문서대로 `pins: ["*:stable"]`을
설정한 운영자의 정착 태그가 다음 스케줄 GC에서 전부 삭제된다 — 이 use case를 지키려고 만든
기능이 그 use case를 부순다.

- POST/DELETE body에 `{"pattern": "..."}` 수용, pin bucket 값을 `[]byte{1}` 대신
  `PinEntry{by, at}`(index.go:128-135), GET은 구조화된 entry 반환.
- 평가는 `doublestar.Match`를 Record.Ref/Tag에 적용(이미 vendored — rewrite.go에서 사용 중).
- 패턴을 계속 미지원할 거면 glob 문법 문자열을 **거부**해야 한다(지금은 받고 조용히 영원히 미매치).
- effort: small-medium.

---

## 2단계 — 운영 가시성

"새벽 3시 질문"에 답하는 읽기 API들. 대부분 이미 있는 내부 상태의 passthrough라 effort small.

### 2.1 `GET | DELETE /v1/store/{name}/image` — 이미지 인벤토리

노드에 뭐가 있는지 아는 유일한 방법이 현재 "GC dry-run을 돌려 keep/delete 사유를 파싱"이다.
retention `Record`(record.go:10-19)는 이미 JSON 태그까지 있는데 API로 안 나간다.

- GET: `s.gc.Index().List(name)` passthrough (ref, repo, tag, digest, last_used,
  last_distributed, first_seen, pinned). `?repo=` `?pinned=` `?ref=` 필터.
- DELETE `?ref=`: 엔진은 안 건드리고 고아 레코드만 `Index.Delete` — 0단계 3의 escape hatch.
- 게이트는 기존 `gcReady()`(handlers_gc.go:254) 패턴(404/501).
- registry store의 `?ref=` digest resolve(캐시에 X가 digest Y로 있나)는 **후속으로 분리** —
  bbolt의 믿음과 라이브 remote resolve를 한 경로에 섞지 않는다.

### 2.2 `GET /v1/gc` — GC 스케줄러 상태

`lastRun`/`nextWake`/grace가 Manager 필드에만 있어(manager.go:38-39, :182) "GC가 언제 도나,
왜 안 도나"가 관측 불가. 특히 **grace가 기본 max_age**라 재시작마다 age-GC가 최대 30일
조용히 no-op이 되는데 밖에서 알 길이 없고, "GC가 2×interval 동안 안 돌았다" 알림도 못 만든다.

- 응답: `{enabled, started, last_run, next_wake, grace_until, running, default_policy,
  schedule{interval, min_interval, grace}}`.
- Manager에 `Status()` 추가 — started/lastRun이 현재 unlocked write(manager.go:176, :218)라
  **mutex 도입 필요**. armed timer deadline은 nextWake 계산 지점(manager.go:182)에서 기록.
- per-engine 카운트(records/pinned/in_use/delete_candidates)는 라이브 데몬을 찌르므로
  `?detail=`로 opt-in 또는 마지막 스케줄러 decision 캐시.
- fleet-wide `POST /v1/gc`는 **뺀다** — per-store POST가 이미 수동 트리거고(Interval<=0에서도 동작,
  handlers_gc.go:115-117), 스케줄러 내부 `gcAll`을 HTTP에서 부르면 lastRun race.

### 2.3 `GET /v1/store/{name}/inuse` — 라이브 in-use 집합

`Engine.InUse(ctx)`(down.go:48-49, docker/containerd 모두 구현)가 GC plan마다 조회되면서
API로는 안 나간다. keep 사유 1순위("in_use")의 근거 데이터를 직접 보여준다.

- handleStorePull 미러: `s.stores.Engine(name)` → `eng.InUse(ctx)` → 정렬된 리스트.
  키가 태그 ref와 sha256 ID 혼합이므로 응답에서 구분 표기. gc-nil 게이트 불필요(retention
  꺼져 있어도 동작).

### 2.4 `GET /v1/store/{name}/watcher` — usage-watcher 상태

`Manager.watch`(manager.go:82-104)가 SeedUsage/WatchUsage 에러를 `_ =`로 삼키고 2s 루프로
조용히 재접속한다. 이벤트 스트림이 죽으면 LastUsed가 무기한 동결 → age-GC가 점점 공격적이 되다
**핫한 이미지를 지우는** 시나리오(air-gap 노드는 재pull도 불가)가 며칠씩 잠복 가능.
`/health`는 데몬 도달성만 보므로 이 고장은 어디에도 안 보인다.

- Manager에 mutex-guarded `map[string]*WatcherStatus` — `{connected, last_event_at,
  last_seed_at, reconnect_count, last_error}`. watch 루프 안에서 stamp.
- §2.6의 OTel gauge(`connected`)로도 같은 데이터를 push — 알림은 그쪽으로.

### 2.5 `GET /readyz` — 집계 readiness

`/healthz`는 상수 "ok"고, `GET /v1/store`는 registry를 **무조건 ready:true 하드코딩**
(store.go StoreStatuses). 구현돼 있는 `pingRegistry`(health.go:164-181)는 인증 필요한
per-store 경로로만 도달 가능 — Nomad service check가 "캐시 registry 죽음"으로 트래픽을
못 끊는다.

- `health.Checker.Check`(singleflight+TTL 캐시 기존) fan-out을 **동시 실행**으로 집계,
  200/503 + `?verbose=1`에 per-store 리스트. `isPublic()`에 추가(auth.go:39).
- store.Set에 `Names()` accessor 추가(EngineNames만 있음, store.go:114).
- readiness 정책 knob 필요: 의도적으로 안 닿는 upstream 때문에 영구 503이 되지 않게
  (기본: 엔진 + 로컬 registry만 평가).

### 2.6 메트릭 — `/metrics` 대신 mkot OTLP push (결정)

**현황**: mkot은 config-driven이라 otel 블록 미설정 = export 없음이 **의도된 기본값**이고,
지금 gantry는 그 상태로 정상이다. 갭은 opt-in 수단 쪽에 있다: 레지스트리에 등록된 exporter가
pretty(LogExporter 전용)뿐이라 **설정을 해도 metric을 내보낼 exporter가 없다**. mkot README가
보여주는 `github.com/lesomnus/mkot/exporters/otlp`는 현재 mkot HEAD(66886cdbecf0, gantry가
핀한 그 커밋)에 존재하지 않는다 (module zip에 exporters/ 없음, module query "no matching
versions"). fleet 관측 파이프라인은 이미 노드 로컬 otel collector → ClickHouse/Grafana이므로
Prometheus scrape를 도입하면 이중 파이프라인이 된다 → 내보낼 때는 OTLP push로 통일.

작업:

1. **mkot에 `exporters/otlp` 모듈 구현** (mkot repo). 선례는 mkot/pretty:
   `init()`에서 `mkot.DefaultExporterRegistry.Set("otlp", ...)`, `mkot.UnimplementedExporterConfig`
   embed, `MetricExporter(ctx)`에서 `otlpmetricgrpc.New` (config.go:156-177 인터페이스).
   `SpanExporter`/`LogExporter`도 같이 구현하면 trace/log 파이프라인까지 한 번에 해결.
   yaml surface는 collector 관례를 따름: `endpoint`, `insecure`, `headers`, `timeout`,
   (metric은) `temporality`.
2. **gantry 배선**: `cmd/config/otel.go`에 blank import 한 줄이 전부 — export 여부는
   지금처럼 배포 config의 opt-in으로 남는다. 샘플 gantry.yaml에는 주석 예시로:
   ```yaml
   otel:
     exporters:
       otlp: { endpoint: "127.0.0.1:4317", insecure: true }
     providers:
       meter:  { exporters: [otlp] }
       tracer: { exporters: [otlp] }
   ```
3. **계측 추가** (scrape 엔드포인트 항목에서 살리는 부분):
   - queue depth/capacity — `len(w.jobs)/cap(w.jobs)` ObservableGauge(warm.newMetrics 확장,
     warm.go:37). 포화가 지금은 503 ErrQueueFull로만 관측된다.
   - jobs-by-state — 같은 callback에서 `Store.List`.
   - retention record/pin counts per engine — `Index.List/Pins`(index.go:96, :147),
     NewManager에 metrics struct.
   - health probe latency — `Checker.runProbe`가 이미 LatencyMS 계산(health.go:130).
   - watcher connected — §2.4의 상태 플래그를 gauge로.
4. GC 결과 카운터(deleted/untagged/errors)는 §5 이벤트 로그와 같은 emit 지점에서.

### 2.7 `GET /v1/version`

`version.Get()`(cmd/version) passthrough `{version, git_rev, git_dirty}`. 롤링 업그레이드 중
어느 노드가 어느 빌드인지 원격 인벤토리. Auth 뒤에 유지. 부수 작업: otel resource attr에도
git_rev 추가(dirty/rebuild 구분). effort: 한 시간.

---

## 3단계 — verify 표면

kamino 운용(CA-leaf-over-TPM 로테이션, referrer promotion gate)에 직결되는 읽기/제어 API.

### 3.1 `POST /v1/verify` — preflight

`Verifier.Verify`(verify.go:28-36)는 self-contained(태그 resolve + referrer 검증 + digest 반환)
인데 job 어드미션 안에서만 돈다. kamino가 재서명 referrer를 zot에 push한 뒤 "엣지가 이걸
받아줄까"를 확인하는 유일한 방법이 현재는 **진짜 move job을 던져 422를 보는 것**(바이트 이동
+ job 레코드 + dedup 오염 동반).

- body `{ref, from}` → `{verified, digest, mode}`. 422 매핑은 handleCreateJob(handlers.go:59-63)과 공유.
- verifier가 현재 Warmer 전용 필드(SetVerifier)이므로 server.New에도 주입(cmd/serve.go:74-78).
- effort: small (~하루).

### 3.2 `GET /v1/verify` — 유효 정책/trust anchor 소개

trust store가 시작 시 1회 파싱(notation.go:161-214)되고 유효 mode는 global+per-store 합성
(serve.go:85-93)인데 어느 것도 조회 불가 — "전 엣지가 새 kamino CA를 신뢰하나"가 노드별 SSH.
**anchor의 notAfter 만료 감시**가 핵심 부가가치(require 모드에서 CA 만료 = 전 job 422 hard outage).

- 응답: provider, global/per-store mode, level, policy scopes/identities,
  anchors `{subject, sha256 fingerprint, not_after}` — 키 재료는 절대 미포함.
- notaryVerifier에 `Describe()` 스냅샷 메서드, 비활성 시 404 대신 `mode: "off"` + 빈 anchors.

### 3.3 `POST /v1/verify/reload` — truststore 핫 리로드

리로드 경로가 전무(SIGHUP도 없음) → CA 로테이션 = 전 fleet gantry 재시작(in-flight job과
in-memory 이력 소멸 동반).

- `verify.New`는 config+disk의 순수 함수라 재호출 가능. `atomic.Pointer[Verifier]`를 든
  Swappable wrapper를 Warmer/server 양쪽에 한 번 주입, Reload는 **성공 시에만 swap**
  (실패 시 기존 verifier 유지 + 422로 사유 반환). in-flight Verify는 옛 verifier로 무해하게 완료.
- effort: small (~200-300 LOC / 4 pkg).

### 3.4 JobSnapshot에 `verification` 객체

검증된 digest가 slog에만 남는다(warm.go:239-241). CI가 "digest X가 fleet CA 서명으로
노드 A,B,C에 배포됐다"를 증빙할 수 없다 — kamino provenance의 감사 프리미티브.

- `Verifier.Verify`가 `Result{Mode, Digest}` 반환하도록 확장(구현체 1개 + 테스트 fake만),
  Job에 stamp, `snapshot()`(job.go:157) 한 곳 수정으로 get/list/progress 전부 커버.
- 1.1(digest 앵커링)과 같은 데이터가 흐르므로 그 작업에 얹으면 공짜에 가깝다.

---

## 4단계 — job 라이프사이클 완성

### 4.1 `POST /v1/job/plan` — dry-run 어드미션

`plan()`(warm.go:178-273)이 이미 전부 계산한다 — 노출만 하면 된다. 죽이는 dead-end 세 개:
① 400 "no rewrite rule matched"의 디버깅 수단이 SSH로 glob 암산뿐, ② proxy×verify 거부가
malformed body와 구분 불가한 generic 400(warm.go:236-238), ③ kamino push 후 검증 확인이
실 job 제출뿐(→3.1과 상보).

- `Warmer.Plan(ctx, Request)` exported wrapper + `PlanResult`(store 바인딩, rewrite된 ref,
  per-engine pull ref, verified digest, would_coalesce). rewrite에 per-rule 평가 trace,
  거부 사유를 typed로.
- 주의: plan()이 현재 `w.rootCtx()`로 검증 — handler에선 request ctx 사용.

### 4.2 `POST /v1/job/{id}/retry`

엣지 링크에선 transient 실패가 기본값인데 layer 하나 실패가 job 전체를 죽이고(warm.go:405-417),
복구는 "원래 JSON body를 기억으로 재구성해 re-POST". retry/backoff는 코드 어디에도 없다.

- **저장된 jobExec 재실행 금지** — src가 원 검증 시점 digest로 핀돼 있어 재검증 없이 stale
  digest를 재복사하게 된다. 대신: `Job`에 원본 `Request` 보관 → `Warmer.Retry(id)` =
  terminal 확인(아니면 409) 후 `Submit(j.req)` — fresh resolve + fresh notation 재검증 +
  기존 dedup/queue-full 경로 그대로.
- `force`(dedup 우회)는 v1에서 뺀다 — active twin과 같은 dst 태그에 writer 둘이 된다.
  cancel-then-retry가 force의 경로.

### 4.3 create 멱등성 가시화

coalesce돼도 202 + Location이 나가 CI가 created/coalesced를 구분 못 하고(handlers.go:71-72),
dedup은 in-flight만 커버해서(store.go:69-71) **타임아웃 후 재시도 POST가 전체 복사를 한 번 더**
돌린다 — mutable tag면 방금 배포한 것과 **다른 digest**가 될 수 있다.

- 응답: created=202 / coalesced=200 + body에 `{coalesced, dedup_key}`.
- `Idempotency-Key` 헤더 → jobID 예약 map(TTL=JobTTL, memStore.mu 아래 예약; 0단계 1의
  sweeper가 함께 청소). 재시작 비영속은 job store 자체가 ephemeral이므로 수용.

### 4.4 `GET /v1/job` 목록 제어

- `?limit=`, `?since=` (List가 이미 CreatedAt desc 정렬이라 truncate만).
- `?state=` 검증 — 지금은 `?state=complete` 오타가 조용히 `[]`를 반환해 "job 없음"과 구분
  불가(자동화에 독). unknown 값은 400.
- bulk DELETE는 **뺀다** — 0단계 1의 sweeper가 근본 해결.

### 4.5 `POST /v1/job/{id}/cancel` — cancel과 evict 분리

현재 DELETE가 취소+제거 원자라(store.go:91-101) JobCanceled 종단 스냅샷이 **허공에
쓰인다**(finish의 store.Update가 이미 지워진 ID에 실패). 새벽에 wedged pull을 취소하는 순간
"어느 layer에서 얼마나 가다 멈췄나"라는 증거가 함께 소멸 — cancel-and-inspect가 정상
워크플로다.

- `j.Cancel()`은 store lock 밖에서 race-free(job.go:66-72). 202 + 현재 스냅샷, terminal이면 409.
- 동반 수정: canceled job을 `memStore.Active`에서 제외 — 안 그러면 cancel 직후 재제출이
  죽어가는 job에 coalesce된다. DELETE 문서는 "evict"로 좁힘.
- effort: ~60-80 LOC.

---

## 5단계 — `GET /v1/event` 감사 로그 (재시작 내구 이력)

엣지 노드는 상시 재시작(Nomad reschedule, 전원, tegra RTC 부팅)인데 memStore가 유일한 job
store라 "배포 이미지가 리부트 전에 도착했었나"가 API로 답 불가. 스케줄 GC의 Decision도 실행
후 버려진다(onRun은 테스트 훅, manager.go:41).

- 새 `internal/event`: bbolt append-only ring (bucket "evt", key=NextSequence,
  count-cap으로 oldest 삭제). **retention DB에 얹지 말 것** — retention 비활성 배포에서
  감사 로그가 조용히 사라진다. 독립 `serve.events.path`(미설정 시 비활성)로.
- emit 지점: job admitted(verified digest 포함 — 3.4가 선행), job terminal(state/err/
  transfers/bytes), GC apply(Decision.Delete + ApplyResult — onRun을 프로덕션 callback으로
  승격), 수동 pull/remove, pin add/remove.
- `GET /v1/event?type=&store=&ref=&state=&since=&limit=` — reverse cursor scan + post-filter.
- 마지막에 두는 이유: emit 지점이 1–4단계 전반에 걸쳐 있어, 먼저 만들면 두 번 배선하게 된다.
- effort: medium (새 소패키지 + emit ~6곳 + handler).

---

## 구현 현황

**0단계 + §2.6 exporter 구현 완료 (2026-07-02).** race/vet clean.

- [x] **0-1 sweeper** — `Warmer.Start`가 JobTTL>0이면 sweeper goroutine 기동(`min(ttl/4, 1m)` tick);
  새 `stop` 채널을 `Stop()`이 닫음. 테스트: terminal job만 TTL 후 evict.
- [x] **0-2 finish/run** — `errors.Is(err, context.Canceled)`만 canceled로 매핑(자기-취소 절 제거);
  layer 실패는 JobFailed + j.Err. 테스트: 자기-취소 후 finish가 failed 기록.
- [x] **0-3 Remove NotFound 관용** — docker `client.IsErrNotFound` / containerd `cerrdefs.IsNotFound`
  → 빈 결과 + nil (idempotent delete; GC가 고아 레코드 self-heal). 테스트: httptest 가짜 데몬(404) +
  bufconn gRPC(codes.NotFound).
- [x] **0-4 `Decision.NextAgeOut`** — exported 필드 + `json:"next_age_out,omitzero"`(메서드 제거);
  swagger 재생성. 테스트: 직렬화/생략.
- [x] **0-5 pull stamp** — `handleStorePull` 성공 시 `gc.Distributed(engine, ref, now)`. 테스트:
  가짜 docker 데몬으로 pull 성공 → index에 LastDistributed 레코드 확인.
- [x] **§2.6 otlp exporter** — `internal/otlp`(mkot/otel만 의존; `mkot/exporters/otlp`로 추출 가능
  형태). `DefaultExporterRegistry.Set("otlp")`; **MetricReader가 PeriodicReader를 component로 반환**
  → `resolver.Shutdown`(=otx Shutdown)이 마지막 collect를 flush(디버그 exporter의 exporter-as-component
  패턴은 flush 없이 닫힘). Span/Log는 sending_queue의 0값을 SDK 기본값으로 보존(mkot `QueueConfig.Build*`는
  0을 그대로 넘겨 batcher가 전부 drop — 상류 버그, mkot 추출 시 함께 수정). config surface:
  `endpoint`(필수)/`tls`(mkot ClientTlsConfig)/`headers`/`timeout`/`interval`/`temporality`/`sending_queue`.
  `cmd/config/otel.go`: blank import + **모든 provider에 `resource/gantry` processor 자동 prepend**
  (사용자 정의 provider가 service.name을 잃지 않게; 명시된 경우 스킵). gantry.yaml에 주석 예시.
  테스트: yaml 디코드, validate, delta selector, 가짜 collector로 E2E push, Build 통합(등록→리소스→
  Shutdown flush).
- [x] **적대 리뷰 라운드** (멀티에이전트, 15건 확정 → 5개 고유 결함 수정):
  - copyLayers dispatch 루프가 `ctx.Err()`를 반환하며 firstErr를 버림(0-2 수정의 잔여 구멍;
    layer 수 > MaxConcurrentLayers면 사실상 결정적으로 재현) → ctx.Done 분기에서 wg.Wait 후
    비-취소 firstErr 우선 반환 + fake Source 회귀 테스트.
  - **[critical]** `otlptracegrpc.New(ctx)`는 self-start인데 `cmd/config.go:65`의 `o.Start`(=resolver.Start)가
    재시작 → "already started"로 tracer 배선 시 기동 실패 → `NewUnstarted` + component가 Start 위임.
  - span/log batch processor가 shutdown에서 flush 안 됨(마지막 5s/1s 텔레메트리 유실) →
    component wrapper의 Shutdown이 processor.Shutdown(드레인 후 exporter 닫음)에 위임 —
    metric의 PeriodicReader 패턴과 대칭. trace/log E2E flush 테스트.
  - mkot `ClientTlsConfig.Build`가 CA pool을 **ClientCAs**(서버측)에 넣어 ca_file/ca_pem이
    무시됨 → creds()에서 RootCAs로 이동(워크어라운드; 상류 수정 후 제거). 자가서명 CA TLS 테스트.
  - sweeper/종료가 job ctx를 cancel하지 않아 base ctx에 cancelCtx 자식 누적 →
    `Warmer.run`에 `defer job.Cancel()` + `memStore.Sweep`이 evict 전 Cancel.
- [x] **mkot 상류 수정 완료 (2026-07-02, /workspaces/github.com/lesomnus/mkot 워킹트리)** — 리포에
  이미 있던 미공개 중첩 모듈 `mkot/otlp`(README의 `exporters/otlp`가 아님; 경로 수정함)를 발견,
  거기에 수정을 얹음: ① tls.go `ClientCAs`→`RootCAs`, ② queue.go 0값 가드,
  ③ `mkot.SpanComponent`/`LogComponent` 공유 헬퍼(component.go) — processor-drain Shutdown +
  Start 위임, ④ debug: sending_queue 명시 설정 없으면 동기 출력(베이스라인부터 깨져 있던
  기존 테스트가 이걸로 복구), MetricReader 전환, ⑤ otlp: MetricReader 전환 + interval/temporality
  추가 + 선언만 되고 무시되던 headers/retry/keepalive/wait_for_ready/balancer_name 배선.
  적대 리뷰 2라운드에서 5건 추가 수정: **delta 셀렉터가 gauge를 delta로 보내 산발 기록 gauge가
  증발**(SDK `metric.DeltaTemporalitySelector`로 교체 — gantry internal/otlp에도 동일 수정),
  retry의 multiplier/randomization_factor는 exporter가 표현 불가 → 조용히 버리는 대신 에러,
  keepalive `time: 0`이 grpc 최소 10s로 클램프되며 몰래 켜지는 것 가드, debug의 부분 설정된
  sending_queue를 opt-in으로 존중, otlp/go.mod 버전 skew 문서화.
- [x] **gantry → mkot/otlp 전환 완료 (2026-07-02)**. mkot 2198e78 푸시 후: gantry go.mod를
  mkot/{,otlp,pretty}@v0.0.0-20260702145326-2198e788ed64로 bump, otel.go blank import를
  `github.com/lesomnus/mkot/otlp`로 교체, `internal/otlp` 삭제(RootCAs 워크어라운드 포함 소멸).
  gantry.yaml 예시를 mkot/otlp surface에 맞춤(headers는 name/value 리스트, timeout 필드 없음,
  retry_on_failure 있음). cmd/config의 Build 통합 테스트가 공개 모듈 기준으로 통과.
- **남은 것**: ① mkot 워킹트리의 `otlp/go.mod` bump(mkot require → 2198e78 pseudo-version)
  커밋/푸시 — 이거 전까지 `go get github.com/lesomnus/mkot/otlp`만 단독으로 쓰는 소비자는
  MVS로 root를 함께 올려줘야 컴파일됨(gantry는 이미 그렇게 함). CI에 `GOWORK=off go build`
  per-module 추가 권장. ② §2.6-3 계측 추가(큐 깊이, jobs-by-state, retention 카운트,
  health latency)는 미착수. ③ pretty 모듈 테스트는 이 작업 전부터 실패 상태(otx ingress/egress
  렌더링 WIP) — 무관.

## 채택하지 않은 것

| 제안 | 사유 |
|---|---|
| `GET /metrics` (Prometheus scrape) | OTLP push로 대체(§2.6) — fleet 파이프라인이 OTel→ClickHouse라 scrape는 이중 파이프라인. 전제(“기존 prometheus exporter 브릿지”)도 오류였음 — exporter가 애초에 없다 |
| `POST /v1/store/{name}/reconcile` (선언형 이미지 셋) | §2.1 인벤토리 + 기존 job/remove로 클라이언트가 diff 가능. 서버에 desired-state를 들이면 소유권이 모호해짐 |
| job 완료 webhook/callback | SSE + `?wait=` long-poll로 충분. 엣지에서 아웃바운드 callback은 도달성 문제만 추가 |
| 토큰 관리 API (`/v1/auth/token`) | 정적 토큰 화이트리스트 모델에서 근거 부족. mTLS가 이미 대안 축 |
