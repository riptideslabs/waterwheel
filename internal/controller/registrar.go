// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strconv"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"go.riptides.io/waterwheel/api/v1alpha1"
	"go.riptides.io/waterwheel/internal/identity"
)

// ConditionShadowed says that pods a more specific WorkloadIdentity matches
// too get that one's workload ID.
const ConditionShadowed = "Shadowed"

// AnnotationWorkloadIdentity on a ClusterSPIFFEID names the WorkloadIdentity
// it was written for.
const AnnotationWorkloadIdentity = "waterwheel.riptides.io/workload-identity"

const (
	managedByLabel = "app.kubernetes.io/managed-by"
	managedBy      = "waterwheel-controller"
	classNameLabel = "spire.spiffe.io/class-name"
	// registrarResync repairs ClusterSPIFFEIDs changed or deleted behind
	// Waterwheel's back.
	registrarResync = 5 * time.Minute
	// registrarSettle lets a burst of changes (kubectl apply -f dir/) land
	// before syncing.
	registrarSettle = time.Second
)

var clusterSPIFFEIDList = schema.GroupVersionKind{Group: "spire.spiffe.io", Version: "v1alpha1", Kind: "ClusterSPIFFEIDList"}

var errNoClusterSPIFFEID = errors.New("SPIRE's ClusterSPIFFEID kind isn't installed; install SPIRE with its controller manager")

// IdentityRegistrar registers the WorkloadIdentities in effect with SPIRE,
// as one ClusterSPIFFEID per selector entry, and keeps their status. It syncs
// everything at once: on start, after changes, and every few minutes.
// ClusterSPIFFEIDs are cluster-scoped, so they can't be owned by a
// WorkloadIdentity; the registrar deletes the ones it labeled and no longer
// wants.
type IdentityRegistrar struct {
	// Client writes ClusterSPIFFEIDs and statuses.
	Client client.Client
	// Reader lists WorkloadIdentities; uncached, so a cache that hasn't
	// synced yet can't make every registration look unwanted.
	Reader          client.Reader
	Changes         *Broadcaster
	SystemNamespace string
	TrustDomain     string
	// ClassName is the spire-controller-manager class of the ClusterSPIFFEIDs.
	ClassName string
	// Recorder gets an event for each condition change.
	Recorder events.EventRecorder
}

// Start runs the sync loop; on the leader only.
func (r *IdentityRegistrar) Start(ctx context.Context) error {
	for {
		changed := r.Changes.Changed()

		if err := r.Sync(ctx); err != nil && ctx.Err() == nil {
			slog.Error("registering WorkloadIdentities with SPIRE", "err", err)
		}

		select {
		case <-ctx.Done():
			return nil
		case <-changed:
		case <-time.After(registrarResync):
		}

		select {
		case <-ctx.Done():
			return nil
		case <-time.After(registrarSettle):
		}
	}
}

// NeedLeaderElection is true: one writer.
func (r *IdentityRegistrar) NeedLeaderElection() bool { return true }

// Sync writes the ClusterSPIFFEIDs of the identities in effect, deletes the
// others Waterwheel wrote, and updates every WorkloadIdentity's status.
func (r *IdentityRegistrar) Sync(ctx context.Context) error {
	var list v1alpha1.WorkloadIdentityList
	if err := r.Reader.List(ctx, &list); err != nil {
		return err
	}

	set := identity.Build(list.Items, r.SystemNamespace)
	failed := map[types.NamespacedName]error{}
	wanted := map[string]bool{}

	for _, id := range set.Identities {
		for _, obj := range r.ClusterSPIFFEIDs(id) {
			wanted[obj.GetName()] = true

			err := r.Client.Apply(ctx, client.ApplyConfigurationFromUnstructured(obj),
				client.FieldOwner(managedBy), client.ForceOwnership)
			if err != nil && failed[id.Key()] == nil {
				failed[id.Key()] = registrationError(err)
			}
		}
	}

	return errors.Join(r.prune(ctx, wanted), r.updateStatus(ctx, list.Items, set, failed))
}

func registrationError(err error) error {
	if meta.IsNoMatchError(err) {
		return errNoClusterSPIFFEID
	}

	return err
}

// prune deletes the ClusterSPIFFEIDs Waterwheel wrote and no longer wants.
func (r *IdentityRegistrar) prune(ctx context.Context, wanted map[string]bool) error {
	var existing unstructured.UnstructuredList
	existing.SetGroupVersionKind(clusterSPIFFEIDList)

	err := r.Client.List(ctx, &existing, client.MatchingLabels{managedByLabel: managedBy})
	if meta.IsNoMatchError(err) {
		return nil
	}

	if err != nil {
		return err
	}

	var errs []error

	for i := range existing.Items {
		obj := &existing.Items[i]
		if wanted[obj.GetName()] {
			continue
		}

		if err := r.Client.Delete(ctx, obj); err != nil && !apierrors.IsNotFound(err) {
			errs = append(errs, err)

			continue
		}

		slog.Info("deleted ClusterSPIFFEID", "name", obj.GetName(), "for", obj.GetAnnotations()[AnnotationWorkloadIdentity])
	}

	return errors.Join(errs...)
}

func (r *IdentityRegistrar) updateStatus(ctx context.Context, wids []v1alpha1.WorkloadIdentity, set *identity.Set,
	failed map[types.NamespacedName]error,
) error {
	var errs []error

	for i := range wids {
		w := &wids[i]
		key := types.NamespacedName{Namespace: w.Namespace, Name: w.Name}
		before := slices.Clone(w.Status.Conditions)

		changed := setCondition(w, &w.Status.Conditions, r.ready(key, w, set, failed))

		if winners := set.Shadowed[key]; len(winners) > 0 {
			names := make([]string, 0, len(winners))
			for _, k := range winners {
				names = append(names, k.String())
			}

			changed = setCondition(w, &w.Status.Conditions, metav1.Condition{
				Type: ConditionShadowed, Status: metav1.ConditionTrue, Reason: "MoreSpecificIdentity",
				Message: "pods that " + strings.Join(names, ", ") + " match too get that WorkloadIdentity's workload ID",
			}) || changed
		} else if meta.RemoveStatusCondition(&w.Status.Conditions, ConditionShadowed) {
			changed = true
		}

		if !changed {
			continue
		}

		err := r.Client.Status().Update(ctx, w)

		switch {
		case err == nil:
			recordTransitions(r.Recorder, w, actionRegister, before, w.Status.Conditions)
		// A conflict means it changed meanwhile, and that change brings another sync.
		case !apierrors.IsConflict(err) && !apierrors.IsNotFound(err):
			errs = append(errs, err)
		}
	}

	return errors.Join(errs...)
}

func (r *IdentityRegistrar) ready(key types.NamespacedName, w *v1alpha1.WorkloadIdentity, set *identity.Set,
	failed map[types.NamespacedName]error,
) metav1.Condition {
	c := metav1.Condition{Type: ConditionReady, Status: metav1.ConditionFalse}

	if p, ok := set.Problems[key]; ok {
		c.Reason, c.Message = p.Reason, p.Message

		return c
	}

	if err := failed[key]; err != nil {
		c.Reason, c.Message = "RegistrationFailed", err.Error()

		return c
	}

	c.Status, c.Reason = metav1.ConditionTrue, "Registered"
	c.Message = fmt.Sprintf("%s, through %d ClusterSPIFFEID(s)",
		identity.SPIFFEID(r.TrustDomain, w.Spec.WorkloadID), len(w.Spec.Selectors))

	return c
}

// ClusterSPIFFEIDs renders an identity's ClusterSPIFFEIDs, one per selector
// entry. Only the sidecar's container gets the SVID: it is the only one with
// the Workload API socket, and the only one that should be.
func (r *IdentityRegistrar) ClusterSPIFFEIDs(id *identity.Identity) []*unstructured.Unstructured {
	objs := make([]*unstructured.Unstructured, 0, len(id.Entries))

	for n, e := range id.Entries {
		workload := []string{"k8s:container-name:" + SidecarName}
		podLabels := map[string]any{}

		for k, v := range e {
			switch k {
			case identity.KeyNamespace:
			case identity.KeyPodName:
				workload = append(workload, "k8s:pod-name:"+v)
			case identity.KeyServiceAccount:
				workload = append(workload, "k8s:sa:"+v)
			default:
				podLabels[strings.TrimPrefix(k, identity.KeyLabelPrefix)] = v
			}
		}

		slices.Sort(workload)

		spec := map[string]any{
			"className":                 r.ClassName,
			"spiffeIDTemplate":          "spiffe://{{ .TrustDomain }}/" + id.WorkloadID,
			"workloadSelectorTemplates": toAny(workload),
		}

		if ns, ok := e[identity.KeyNamespace]; ok {
			spec["namespaceSelector"] = map[string]any{"matchLabels": map[string]any{"kubernetes.io/metadata.name": ns}}
		}

		if len(podLabels) > 0 {
			spec["podSelector"] = map[string]any{"matchLabels": podLabels}
		}

		svidProperties(spec, id.SVID)

		obj := &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": "spire.spiffe.io/v1alpha1",
			"kind":       "ClusterSPIFFEID",
			"metadata": map[string]any{
				"name":        clusterSPIFFEIDName(id.Namespace, id.Name, n),
				"labels":      map[string]any{managedByLabel: managedBy, classNameLabel: r.ClassName},
				"annotations": map[string]any{AnnotationWorkloadIdentity: id.Key().String()},
			},
			"spec": spec,
		}}

		objs = append(objs, obj)
	}

	return objs
}

func svidProperties(spec map[string]any, p *v1alpha1.SVIDProperties) {
	if p == nil {
		return
	}

	if p.JWT != nil && p.JWT.TTL != nil {
		spec["jwtTtl"] = p.JWT.TTL.Duration.String()
	}

	if p.X509 == nil {
		return
	}

	if p.X509.TTL != nil {
		spec["ttl"] = p.X509.TTL.Duration.String()
	}

	if len(p.X509.DNSNames) > 0 {
		// They are templates to the controller manager; keep them literal.
		names := make([]string, 0, len(p.X509.DNSNames))
		for _, d := range p.X509.DNSNames {
			names = append(names, strings.ReplaceAll(d, "{{", "{{`{{`}}"))
		}

		spec["dnsNameTemplates"] = toAny(names)
	}
}

// clusterSPIFFEIDName is waterwheel.<namespace>.<name>.<entry>. Namespaces
// have no dots, so names can't collide; a name too long gets hashed.
func clusterSPIFFEIDName(namespace, name string, entry int) string {
	n := "waterwheel." + namespace + "." + name + "." + strconv.Itoa(entry)
	if len(n) <= 253 {
		return n
	}

	sum := sha256.Sum256([]byte(name))

	return "waterwheel." + namespace + "." + hex.EncodeToString(sum[:8]) + "." + strconv.Itoa(entry)
}

func toAny(s []string) []any {
	out := make([]any, len(s))
	for i, v := range s {
		out[i] = v
	}

	return out
}
