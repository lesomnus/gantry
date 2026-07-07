package rpc

import (
	"encoding/json"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/lesomnus/gantry/internal/event"
	"github.com/lesomnus/gantry/internal/health"
	"github.com/lesomnus/gantry/internal/retention"
	"github.com/lesomnus/gantry/internal/store"
	"github.com/lesomnus/gantry/internal/verify"
	"github.com/lesomnus/gantry/internal/warm"
	"github.com/lesomnus/gantry/pb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// nsGantry namespaces the deterministic ids synthesized for resources whose
// domain identity is composite (Image: store/ref, Pin: store/value).
var nsGantry = uuid.NewSHA1(uuid.NameSpaceURL, []byte("gantry.lesomnus.github.com"))

func imageID(storeName, ref string) []byte {
	u := uuid.NewSHA1(nsGantry, []byte("image\x00"+storeName+"\x00"+ref))
	return u[:]
}

func pinID(storeName, value string) []byte {
	u := uuid.NewSHA1(nsGantry, []byte("pin\x00"+storeName+"\x00"+value))
	return u[:]
}

// ts converts a domain time; the zero time maps to an absent field.
func ts(t time.Time) *timestamppb.Timestamp {
	if t.IsZero() {
		return nil
	}
	return timestamppb.New(t)
}

// durStr converts a Duration.String() rendering back to a proto duration; an
// empty or unparseable string maps to an absent field.
func durStr(s string) *durationpb.Duration {
	if s == "" {
		return nil
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return nil
	}
	return durationpb.New(d)
}

// page applies offset-token pagination: the token is the decimal offset into
// the (stable-ordered) full result. Good enough for gantry's list sizes; a
// keyed cursor can replace it without a contract change.
func page[T any](items []T, size int32, token string) ([]T, string, error) {
	if size < 0 {
		return nil, "", status.Error(codes.InvalidArgument, "page_size must not be negative")
	}
	off := 0
	if token != "" {
		n, err := strconv.Atoi(token)
		if err != nil || n < 0 {
			return nil, "", status.Error(codes.InvalidArgument, "invalid page token")
		}
		off = n
	}
	if off >= len(items) {
		return nil, "", nil
	}
	items = items[off:]
	if size <= 0 || int(size) >= len(items) {
		return items, "", nil
	}
	return items[:size], strconv.Itoa(off + int(size)), nil
}

// --- enum maps -------------------------------------------------------------

var jobStateToPB = map[warm.JobState]pb.JobState{
	warm.JobPending:  pb.JobState_JOB_STATE_PENDING,
	warm.JobRunning:  pb.JobState_JOB_STATE_RUNNING,
	warm.JobDone:     pb.JobState_JOB_STATE_DONE,
	warm.JobFailed:   pb.JobState_JOB_STATE_FAILED,
	warm.JobCanceled: pb.JobState_JOB_STATE_CANCELED,
}

var jobStateFromPB = map[pb.JobState]warm.JobState{
	pb.JobState_JOB_STATE_PENDING:  warm.JobPending,
	pb.JobState_JOB_STATE_RUNNING:  warm.JobRunning,
	pb.JobState_JOB_STATE_DONE:     warm.JobDone,
	pb.JobState_JOB_STATE_FAILED:   warm.JobFailed,
	pb.JobState_JOB_STATE_CANCELED: warm.JobCanceled,
}

var transferStateToPB = map[string]pb.TransferState{
	"pending": pb.TransferState_TRANSFER_STATE_PENDING,
	"running": pb.TransferState_TRANSFER_STATE_RUNNING,
	"done":    pb.TransferState_TRANSFER_STATE_DONE,
	"exists":  pb.TransferState_TRANSFER_STATE_EXISTS,
	"failed":  pb.TransferState_TRANSFER_STATE_FAILED,
}

var layerStateToPB = map[string]pb.LayerState{
	"pending": pb.LayerState_LAYER_STATE_PENDING,
	"pulling": pb.LayerState_LAYER_STATE_PULLING,
	"done":    pb.LayerState_LAYER_STATE_DONE,
	"exists":  pb.LayerState_LAYER_STATE_EXISTS,
	"failed":  pb.LayerState_LAYER_STATE_FAILED,
}

var verifyModeToPB = map[string]pb.VerifyMode{
	"off":               pb.VerifyMode_VERIFY_MODE_OFF,
	"verify-if-present": pb.VerifyMode_VERIFY_MODE_VERIFY_IF_PRESENT,
	"require":           pb.VerifyMode_VERIFY_MODE_REQUIRE,
}

var storeKindToPB = map[string]pb.StoreKind{
	"oci":        pb.StoreKind_STORE_KIND_OCI,
	"docker":     pb.StoreKind_STORE_KIND_DOCKER,
	"containerd": pb.StoreKind_STORE_KIND_CONTAINERD,
}

var storeKindFromPB = map[pb.StoreKind]string{
	pb.StoreKind_STORE_KIND_OCI:        "oci",
	pb.StoreKind_STORE_KIND_DOCKER:     "docker",
	pb.StoreKind_STORE_KIND_CONTAINERD: "containerd",
}

var storeModeToPB = map[string]pb.StoreMode{
	"copy":  pb.StoreMode_STORE_MODE_COPY,
	"proxy": pb.StoreMode_STORE_MODE_PROXY,
}

var eventTypeToPB = map[event.Type]pb.EventType{
	event.JobAdmitted: pb.EventType_EVENT_TYPE_JOB_ADMITTED,
	event.JobDone:     pb.EventType_EVENT_TYPE_JOB_DONE,
	event.GCApplied:   pb.EventType_EVENT_TYPE_GC_APPLIED,
	event.ImagePulled: pb.EventType_EVENT_TYPE_IMAGE_PULLED,
	event.ImageRemove: pb.EventType_EVENT_TYPE_IMAGE_REMOVED,
	event.Pinned:      pb.EventType_EVENT_TYPE_PINNED,
	event.Unpinned:    pb.EventType_EVENT_TYPE_UNPINNED,
}

var eventTypeFromPB = map[pb.EventType]event.Type{
	pb.EventType_EVENT_TYPE_JOB_ADMITTED:  event.JobAdmitted,
	pb.EventType_EVENT_TYPE_JOB_DONE:      event.JobDone,
	pb.EventType_EVENT_TYPE_GC_APPLIED:    event.GCApplied,
	pb.EventType_EVENT_TYPE_IMAGE_PULLED:  event.ImagePulled,
	pb.EventType_EVENT_TYPE_IMAGE_REMOVED: event.ImageRemove,
	pb.EventType_EVENT_TYPE_PINNED:        event.Pinned,
	pb.EventType_EVENT_TYPE_UNPINNED:      event.Unpinned,
}

var gcDeleteReasonToPB = map[string]pb.GcDeleteReason{
	"age_exceeded":   pb.GcDeleteReason_GC_DELETE_REASON_AGE_EXCEEDED,
	"max_n_exceeded": pb.GcDeleteReason_GC_DELETE_REASON_MAX_N_EXCEEDED,
	"untagged":       pb.GcDeleteReason_GC_DELETE_REASON_UNTAGGED,
}

var gcKeepReasonToPB = map[string]pb.GcKeepReason{
	"in_use":          pb.GcKeepReason_GC_KEEP_REASON_IN_USE,
	"pinned":          pb.GcKeepReason_GC_KEEP_REASON_PINNED,
	"keep_n_recent":   pb.GcKeepReason_GC_KEEP_REASON_KEEP_N_RECENT,
	"within_max_age":  pb.GcKeepReason_GC_KEEP_REASON_WITHIN_MAX_AGE,
	"grace":           pb.GcKeepReason_GC_KEEP_REASON_GRACE,
	"age_gc_disabled": pb.GcKeepReason_GC_KEEP_REASON_AGE_GC_DISABLED,
	"unmanaged":       pb.GcKeepReason_GC_KEEP_REASON_UNMANAGED,
	"untagged_grace":  pb.GcKeepReason_GC_KEEP_REASON_UNTAGGED_GRACE,
	"digest_tracked":  pb.GcKeepReason_GC_KEEP_REASON_DIGEST_TRACKED,
}

// --- converters ------------------------------------------------------------

func storeByName(name string) *pb.Store {
	if name == "" {
		return nil
	}
	return pb.Store_builder{Name: name}.Build()
}

func statusToPB(st store.Status) *pb.Store {
	return pb.Store_builder{
		Name:      st.Name,
		Kind:      storeKindToPB[st.Kind],
		Host:      st.Host,
		Mode:      storeModeToPB[st.Mode],
		Address:   st.Address,
		Namespace: st.Namespace,
		Ready:     st.Ready,
		Error:     st.Error,
		Capabilities: pb.StoreCapabilities_builder{
			Read:      st.Capabilities.Read,
			Write:     st.Capabilities.Write,
			Pull:      st.Capabilities.Pull,
			Verify:    st.Capabilities.Verify,
			Gc:        st.Capabilities.GC,
			Reconcile: st.Capabilities.Reconcile,
		}.Build(),
	}.Build()
}

func verificationToPB(v *warm.VerificationSnapshot) *pb.Verification {
	if v == nil {
		return nil
	}
	return pb.Verification_builder{
		Mode:     verifyModeToPB[v.Mode],
		Verified: v.Verified,
		Digest:   v.Digest,
	}.Build()
}

func jobToPB(snap warm.JobSnapshot) *pb.Job {
	transfers := make([]*pb.Transfer, 0, len(snap.Transfers))
	for _, t := range snap.Transfers {
		layers := make([]*pb.Layer, 0, len(t.Layers))
		for _, l := range t.Layers {
			layers = append(layers, pb.Layer_builder{
				Digest:   l.Digest,
				Platform: l.Platform,
				Total:    l.Total,
				Done:     l.Done,
				State:    layerStateToPB[l.State],
			}.Build())
		}
		transfers = append(transfers, pb.Transfer_builder{
			Store:      t.Store,
			Kind:       storeKindToPB[t.Kind],
			From:       t.From,
			Ref:        t.Ref,
			Digest:     t.Digest,
			State:      transferStateToPB[t.State],
			BytesTotal: t.BytesTotal,
			BytesDone:  t.BytesDone,
			Layers:     layers,
			Error:      t.Err,
		}.Build())
	}
	b := pb.Job_builder{
		Id:           snap.ID,
		Ref:          snap.Ref,
		Platforms:    snap.Platforms,
		State:        jobStateToPB[snap.State],
		Error:        snap.Err,
		Verification: verificationToPB(snap.Verification),
		Transfers:    transfers,
		CreatedAt:    ts(snap.CreatedAt),
		StartedAt:    ts(snap.StartedAt),
		EndedAt:      ts(snap.EndedAt),
	}
	// The snapshot carries the resolved stores inside its transfer step.
	if len(snap.Transfers) > 0 {
		b.From = storeByName(snap.Transfers[0].From)
		b.To = storeByName(snap.Transfers[0].Store)
	}
	return b.Build()
}

func recordToPB(storeName string, rec retention.Record, inUse bool) *pb.Image {
	return pb.Image_builder{
		Id:              imageID(storeName, rec.Ref),
		Store:           storeByName(storeName),
		Ref:             rec.Ref,
		Repo:            rec.Repo,
		Tag:             rec.Tag,
		Digest:          rec.Digest,
		FirstSeen:       ts(rec.FirstSeen),
		LastUsed:        ts(rec.LastUsed),
		LastDistributed: ts(rec.LastDistributed),
		Pinned:          rec.Pinned,
		InUse:           inUse,
	}.Build()
}

func pinToPB(storeName string, e retention.PinEntry) *pb.Pin {
	return pb.Pin_builder{
		Id:       pinID(storeName, e.Value),
		Store:    storeByName(storeName),
		Value:    e.Value,
		Pattern:  e.Pattern,
		PinnedAt: ts(e.At),
	}.Build()
}

func eventToPB(e event.Event) *pb.Event {
	b := pb.Event_builder{
		Seq:    e.Seq,
		At:     ts(e.At),
		Type:   eventTypeToPB[e.Type],
		Store:  e.Store,
		Ref:    e.Ref,
		State:  jobStateToPB[warm.JobState(e.State)],
		Digest: e.Digest,
		Error:  e.Error,
	}
	if len(e.Detail) > 0 {
		var d struct {
			From     string `json:"from"`
			Job      string `json:"job"`
			Bytes    int64  `json:"bytes"`
			Deleted  int32  `json:"deleted"`
			Untagged int32  `json:"untagged"`
			Reaped   int32  `json:"reaped"`
			Errors   int32  `json:"errors"`
		}
		if json.Unmarshal(e.Detail, &d) == nil {
			b.Detail = pb.EventDetail_builder{
				From:     d.From,
				Job:      d.Job,
				Bytes:    d.Bytes,
				Deleted:  d.Deleted,
				Untagged: d.Untagged,
				Reaped:   d.Reaped,
				Errors:   d.Errors,
			}.Build()
		}
	}
	return b.Build()
}

func reportToPB(rep health.Report) *pb.StoreHealthResponse {
	b := pb.StoreHealthResponse_builder{
		Healthy:   proto.Bool(rep.Healthy),
		LatencyMs: proto.Int64(rep.LatencyMS),
		CheckedAt: ts(rep.CheckedAt),
		Cached:    proto.Bool(rep.Cached),
	}
	if k, ok := storeKindToPB[rep.Kind]; ok {
		b.Kind = &k
	}
	if rep.Error != "" {
		b.Error = proto.String(rep.Error)
	}
	return b.Build()
}

func watcherToPB(ws retention.WatcherStatus) *pb.GcWatcherStatus {
	b := pb.GcWatcherStatus_builder{
		Connected:     proto.Bool(ws.Connected),
		WatchingSince: ts(ws.Since),
		LastEventAt:   ts(ws.LastEventAt),
		LastSeedAt:    ts(ws.LastSeedAt),
		Reconnects:    proto.Int32(int32(ws.Reconnects)),
	}
	if ws.LastError != "" {
		b.LastError = proto.String(ws.LastError)
	}
	return b.Build()
}

func decisionToPB(dec retention.Decision) *pb.StoreGcPlanResponse {
	deletes := make([]*pb.GcCandidate, 0, len(dec.Delete))
	for _, c := range dec.Delete {
		cb := pb.GcCandidate_builder{
			LastUsed: ts(c.LastUsed),
		}
		if c.Ref != "" {
			cb.Ref = proto.String(c.Ref)
		}
		if c.Digest != "" {
			cb.Digest = proto.String(c.Digest)
		}
		if c.ImageID != "" {
			cb.ImageId = proto.String(c.ImageID)
		}
		if r, ok := gcDeleteReasonToPB[c.Reason]; ok {
			cb.Reason = &r
		}
		deletes = append(deletes, cb.Build())
	}
	keeps := make([]*pb.GcKept, 0, len(dec.Keep))
	for _, k := range dec.Keep {
		kb := pb.GcKept_builder{}
		if k.Ref != "" {
			kb.Ref = proto.String(k.Ref)
		}
		if r, ok := gcKeepReasonToPB[k.Reason]; ok {
			kb.Reason = &r
		}
		keeps = append(keeps, kb.Build())
	}
	return pb.StoreGcPlanResponse_builder{
		Delete:     deletes,
		Keep:       keeps,
		NextAgeOut: ts(dec.NextAgeOut),
	}.Build()
}

func applyResultToPB(res retention.ApplyResult) *pb.StoreGcApplyResponse {
	return pb.StoreGcApplyResponse_builder{
		Evaluated: proto.Int32(int32(res.Evaluated)),
		Untagged:  res.Untagged,
		Deleted:   res.Deleted,
		Reaped:    res.Reaped,
		Skipped:   res.Skipped,
		Errors:    res.Errors,
	}.Build()
}

func planToPB(res warm.PlanResult) *pb.JobPlanResponse {
	b := pb.JobPlanResponse_builder{
		Platforms:    res.Platforms,
		Verification: verificationToPB(res.Verification),
	}
	if res.From != "" {
		b.From = proto.String(res.From)
	}
	if res.To != "" {
		b.To = proto.String(res.To)
	}
	if res.SrcRef != "" {
		b.SrcRef = proto.String(res.SrcRef)
	}
	if res.DstRef != "" {
		b.DstRef = proto.String(res.DstRef)
	}
	b.CopyReferrers = proto.Bool(res.CopyReferrers)
	if res.Coalesces != "" {
		b.Coalesces = proto.String(res.Coalesces)
	}
	return b.Build()
}

func describeToPB(d verify.Description, stores map[string]pb.VerifyMode) *pb.VerifyDescribeResponse {
	policies := make([]*pb.VerifyPolicy, 0, len(d.Policies))
	for _, p := range d.Policies {
		pv := pb.VerifyPolicy_builder{
			RegistryScopes:    p.RegistryScopes,
			TrustedIdentities: p.TrustedIdentities,
		}
		if p.Name != "" {
			pv.Name = proto.String(p.Name)
		}
		if p.Level != "" {
			pv.VerificationLevel = proto.String(p.Level)
		}
		policies = append(policies, pv.Build())
	}
	anchors := make([]*pb.VerifyAnchor, 0, len(d.Anchors))
	for _, a := range d.Anchors {
		av := pb.VerifyAnchor_builder{
			NotAfter: ts(a.NotAfter),
		}
		if a.Subject != "" {
			av.Subject = proto.String(a.Subject)
		}
		if a.Fingerprint != "" {
			av.Fingerprint = proto.String(a.Fingerprint)
		}
		anchors = append(anchors, av.Build())
	}
	b := pb.VerifyDescribeResponse_builder{
		Enabled:  proto.Bool(d.Enabled),
		Policies: policies,
		Anchors:  anchors,
		Stores:   stores,
	}
	if d.Provider != "" {
		b.Provider = proto.String(d.Provider)
	}
	if m, ok := verifyModeToPB[d.Mode]; ok {
		b.Mode = &m
	}
	if d.Level != "" {
		b.Level = proto.String(d.Level)
	}
	return b.Build()
}
