package k8s

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/PrPlanIT/StageFreight/src/config"
	"github.com/PrPlanIT/StageFreight/src/gitops"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
	gatewayclient "sigs.k8s.io/gateway-api/pkg/client/clientset/versioned"
)

const (
	maxOwnerRefDepth = 10
	labelInstance    = "app.kubernetes.io/instance"
	labelName        = "app.kubernetes.io/name"
	labelVersion     = "app.kubernetes.io/version"
	labelHelmChart   = "helm.sh/chart"
	labelTierOverride = "narrator.stagefreight.io/tier"
)

// Discover queries the live cluster and returns a complete DiscoveryResult.
// ObservedAt is captured once at start. Uses client-go directly.
func Discover(ctx context.Context, catalogPath, repoRoot string, exposureRules config.ExposureRules) (*DiscoveryResult, error) {
	observedAt := time.Now()

	// Build Kubernetes client
	config, clusterName, err := buildConfig()
	if err != nil {
		return nil, fmt.Errorf("k8s-inventory requires Kubernetes API access at render time; %w", err)
	}

	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		return nil, fmt.Errorf("creating kubernetes client: %w", err)
	}

	gwClient, err := gatewayclient.NewForConfig(config)
	if err != nil {
		return nil, fmt.Errorf("creating gateway-api client: %w", err)
	}

	// Load catalog
	catalog, err := LoadCatalog(catalogPath)
	if err != nil {
		return nil, fmt.Errorf("loading catalog: %w", err)
	}

	// Phase 1: Discover workloads (only seeds)
	groups, err := discoverWorkloads(ctx, clientset)
	if err != nil {
		return nil, err
	}

	// Phase 1.5: Prefix-based family grouping for unlabeled multi-component apps.
	// Only touches name-fallback workloads. Never merges into labeled identities.
	groupByPrefix(groups)

	// Phase 2: Augment with services
	if err := augmentServices(ctx, clientset, groups); err != nil {
		return nil, err
	}

	// Phase 3: Augment with HTTPRoutes
	if err := augmentHTTPRoutes(ctx, gwClient, clientset, groups); err != nil {
		// Gateway API may not be installed — log but don't fail
		_ = err
	}

	// Phase 4: Resolve declared sources from Flux graph (local repo walk)
	fluxSources := resolveFluxSources(repoRoot, groups)

	// Phase 5: Build AppRecords, classify, apply catalog
	resolver := NewCategoryResolver(nil)
	records := buildRecords(groups, catalog, resolver, fluxSources, exposureRules)

	// Phase 6: Separate into tiers
	var apps, platform []AppRecord
	for _, r := range records {
		switch r.Tier {
		case TierApp:
			apps = append(apps, r)
		case TierPlatform:
			platform = append(platform, r)
		case TierHidden:
			// excluded
		}
	}

	sortRecords(apps)
	sortRecords(platform)

	// Phase 7: Reconcile lifecycle state + derive graveyard.
	// Scoped to cluster: .stagefreight/manifests/k8s-inventory-<cluster>.json
	allActive := append(apps, platform...)
	var graveyard []GraveyardEntry

	if repoRoot != "" && clusterName != "" {
		manifest, err := LoadManifest(repoRoot, clusterName)
		if err == nil {
			changed := ReconcileLifecycle(manifest, allActive, true, observedAt)
			if changed {
				_ = SaveManifest(repoRoot, clusterName, manifest)
			}
			graveyard = GraveyardFromManifest(manifest)
		}
	}

	return &DiscoveryResult{
		Apps:       apps,
		Platform:   platform,
		Graveyard:  graveyard,
		ObservedAt: observedAt,
		Cluster:    clusterName,
	}, nil
}

// buildConfig creates a Kubernetes REST config from kubeconfig or in-cluster.
func buildConfig() (*rest.Config, string, error) {
	// Try KUBECONFIG / default kubeconfig path
	rules := clientcmd.NewDefaultClientConfigLoadingRules()
	overrides := &clientcmd.ConfigOverrides{}
	kubeConfig := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(rules, overrides)

	config, err := kubeConfig.ClientConfig()
	if err == nil {
		rawConfig, _ := kubeConfig.RawConfig()
		return config, rawConfig.CurrentContext, nil
	}

	// Fallback: in-cluster
	config, err = rest.InClusterConfig()
	if err != nil {
		return nil, "", fmt.Errorf("no valid kubeconfig or in-cluster credentials available")
	}
	return config, "in-cluster", nil
}

// appGroup accumulates resources for a single app identity during discovery.
type appGroup struct {
	identity   WorkloadIdentity
	workloads  []workloadInfo
	services   []corev1.Service
	routes     []ExposureRef
	podLabels  map[string]string // merged pod template labels from first workload
}

type workloadInfo struct {
	name       string
	kind       string
	replicas   int32
	ready      int32
	containers []corev1.Container
	initContainers []corev1.Container
	labels     map[string]string
}

// discoverWorkloads lists all Deployments, StatefulSets, DaemonSets and
// groups them by resolved app identity.
func discoverWorkloads(ctx context.Context, cs kubernetes.Interface) (map[AppKey]*appGroup, error) {
	groups := map[AppKey]*appGroup{}

	// Deployments
	deploys, err := cs.AppsV1().Deployments("").List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("listing deployments: %w", err)
	}
	for i := range deploys.Items {
		d := &deploys.Items[i]
		id := resolveIdentity(d.Namespace, d.Name, d.Labels, d.OwnerReferences, "Deployment", ctx, cs)
		addWorkload(groups, id, workloadInfo{
			name:           d.Name,
			kind:           "Deployment",
			replicas:       ptr32(d.Spec.Replicas, 1),
			ready:          d.Status.ReadyReplicas,
			containers:     d.Spec.Template.Spec.Containers,
			initContainers: d.Spec.Template.Spec.InitContainers,
			labels:         d.Spec.Template.Labels,
		})
	}

	// StatefulSets
	stss, err := cs.AppsV1().StatefulSets("").List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("listing statefulsets: %w", err)
	}
	for i := range stss.Items {
		s := &stss.Items[i]
		id := resolveIdentity(s.Namespace, s.Name, s.Labels, s.OwnerReferences, "StatefulSet", ctx, cs)
		addWorkload(groups, id, workloadInfo{
			name:           s.Name,
			kind:           "StatefulSet",
			replicas:       ptr32(s.Spec.Replicas, 1),
			ready:          s.Status.ReadyReplicas,
			containers:     s.Spec.Template.Spec.Containers,
			initContainers: s.Spec.Template.Spec.InitContainers,
			labels:         s.Spec.Template.Labels,
		})
	}

	// DaemonSets
	dss, err := cs.AppsV1().DaemonSets("").List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("listing daemonsets: %w", err)
	}
	for i := range dss.Items {
		d := &dss.Items[i]
		id := resolveIdentity(d.Namespace, d.Name, d.Labels, d.OwnerReferences, "DaemonSet", ctx, cs)
		addWorkload(groups, id, workloadInfo{
			name:           d.Name,
			kind:           "DaemonSet",
			replicas:       d.Status.DesiredNumberScheduled,
			ready:          d.Status.NumberReady,
			containers:     d.Spec.Template.Spec.Containers,
			initContainers: d.Spec.Template.Spec.InitContainers,
			labels:         d.Spec.Template.Labels,
		})
	}

	return groups, nil
}

// resolveIdentity determines the app identity for a workload.
// Strict precedence, frozen once resolved.
func resolveIdentity(ns, name string, labels map[string]string, owners []metav1.OwnerReference, kind string, ctx context.Context, cs kubernetes.Interface) WorkloadIdentity {
	// 1. app.kubernetes.io/instance
	if v, ok := labels[labelInstance]; ok && v != "" {
		return WorkloadIdentity{
			Key:    AppKey{Namespace: ns, Identity: v},
			Source: "label/instance",
			RootUID: resolveRootUID(ns, owners, ctx, cs),
		}
	}

	// 2. app.kubernetes.io/name
	if v, ok := labels[labelName]; ok && v != "" {
		return WorkloadIdentity{
			Key:    AppKey{Namespace: ns, Identity: v},
			Source: "label/name",
			RootUID: resolveRootUID(ns, owners, ctx, cs),
		}
	}

	// 3. helm.sh/chart family
	if v, ok := labels[labelHelmChart]; ok && v != "" {
		// chart label is "name-version", extract name
		chartName := v
		if idx := strings.LastIndex(v, "-"); idx > 0 {
			chartName = v[:idx]
		}
		return WorkloadIdentity{
			Key:    AppKey{Namespace: ns, Identity: chartName},
			Source: "helm",
			RootUID: resolveRootUID(ns, owners, ctx, cs),
		}
	}

	// 4. ownerRef root workload name
	if rootName, rootUID := walkOwnerRefs(ns, owners, ctx, cs); rootName != "" {
		return WorkloadIdentity{
			Key:     AppKey{Namespace: ns, Identity: rootName},
			Source:  "ownerRef",
			RootUID: rootUID,
		}
	}

	// 5. workload name fallback
	return WorkloadIdentity{
		Key:    AppKey{Namespace: ns, Identity: name},
		Source: "name",
	}
}

// resolveRootUID walks ownerRefs to find the root UID for collision detection.
func resolveRootUID(ns string, owners []metav1.OwnerReference, ctx context.Context, cs kubernetes.Interface) string {
	_, uid := walkOwnerRefs(ns, owners, ctx, cs)
	return uid
}

// walkOwnerRefs traverses ownerReferences to the root controller.
// Guards against cycles (visited set) and depth (maxOwnerRefDepth).
func walkOwnerRefs(ns string, owners []metav1.OwnerReference, ctx context.Context, cs kubernetes.Interface) (string, string) {
	visited := map[types.UID]bool{}
	currentOwners := owners
	currentNS := ns
	var lastName, lastUID string

	for depth := 0; depth < maxOwnerRefDepth && len(currentOwners) > 0; depth++ {
		// Find the controller owner
		var controller *metav1.OwnerReference
		for i := range currentOwners {
			if currentOwners[i].Controller != nil && *currentOwners[i].Controller {
				controller = &currentOwners[i]
				break
			}
		}
		if controller == nil {
			break
		}

		uid := controller.UID
		if visited[uid] {
			break // cycle detected
		}
		visited[uid] = true
		lastName = controller.Name
		lastUID = string(uid)

		// Walk up: try to get the owner's ownerReferences
		switch controller.Kind {
		case "ReplicaSet":
			rs, err := cs.AppsV1().ReplicaSets(currentNS).Get(ctx, controller.Name, metav1.GetOptions{})
			if err != nil {
				return lastName, lastUID
			}
			currentOwners = rs.OwnerReferences
		case "Deployment":
			return controller.Name, string(controller.UID)
		case "StatefulSet":
			return controller.Name, string(controller.UID)
		case "DaemonSet":
			return controller.Name, string(controller.UID)
		default:
			return lastName, lastUID
		}
	}

	return lastName, lastUID
}

// addWorkload adds a workload to the appropriate app group, handling collision detection.
func addWorkload(groups map[AppKey]*appGroup, id WorkloadIdentity, wl workloadInfo) {
	key := id.Key

	existing, ok := groups[key]
	if ok && id.RootUID != "" && existing.identity.RootUID != "" && id.RootUID != existing.identity.RootUID {
		// Collision: same identity but different root owner.
		// Disambiguate with short UID suffix.
		key.Identity = key.Identity + "#" + id.RootUID[:8]
		id.Key = key
	}

	g, ok := groups[key]
	if !ok {
		g = &appGroup{
			identity:  id,
			podLabels: wl.labels,
		}
		groups[key] = g
	}

	g.workloads = append(g.workloads, wl)
}

// genericPrefixDenylist contains tokens that are too generic to be family prefixes.
var genericPrefixDenylist = map[string]bool{
	"api": true, "web": true, "app": true, "service": true,
	"backend": true, "frontend": true, "worker": true, "server": true,
	"db": true, "cache": true, "proxy": true, "ingress": true,
}

// groupByPrefix merges unlabeled multi-component workloads into family groups.
// Only touches workloads with Source == "name" (label-based identities are frozen).
// Never merges into existing labeled identities.
// Prefix selection: shortest stable prefix, ≥2 matches, non-empty suffixes, denylist enforced.
func groupByPrefix(groups map[AppKey]*appGroup) {
	// Collect name-fallback groups per namespace.
	type candidate struct {
		key   AppKey
		group *appGroup
	}
	byNS := map[string][]candidate{}

	for key, g := range groups {
		if g.identity.Source == "name" {
			byNS[key.Namespace] = append(byNS[key.Namespace], candidate{key: key, group: g})
		}
	}

	for _, candidates := range byNS {
		if len(candidates) < 2 {
			continue
		}

		// Find prefix families.
		// For each candidate, extract potential prefix (everything before last "-" segment).
		prefixMembers := map[string][]candidate{}
		for _, c := range candidates {
			prefix := extractPrefix(c.key.Identity)
			if prefix == "" || genericPrefixDenylist[prefix] {
				continue
			}
			prefixMembers[prefix] = append(prefixMembers[prefix], c)
		}

		// Merge groups that share a prefix with ≥2 members.
		for prefix, members := range prefixMembers {
			if len(members) < 2 {
				continue
			}

			// Ensure prefix doesn't collide with an existing labeled identity.
			familyKey := AppKey{Namespace: members[0].key.Namespace, Identity: prefix}
			if existing, ok := groups[familyKey]; ok && existing.identity.Source != "name" {
				continue // labeled identity exists — never merge into it
			}

			// Create or find the family group.
			family, exists := groups[familyKey]
			if !exists {
				family = &appGroup{
					identity: WorkloadIdentity{
						Key:    familyKey,
						Source: "prefix",
					},
				}
			}

			// Merge members into family.
			for _, m := range members {
				// Move workloads to family.
				for _, wl := range m.group.workloads {
					family.workloads = append(family.workloads, wl)
				}
				// Move routes.
				family.routes = append(family.routes, m.group.routes...)
				// Move services.
				family.services = append(family.services, m.group.services...)
				// Inherit first pod labels if family has none.
				if family.podLabels == nil && m.group.podLabels != nil {
					family.podLabels = m.group.podLabels
				}

				// Remove old group.
				if m.key != familyKey {
					delete(groups, m.key)
				}
			}

			groups[familyKey] = family
		}
	}
}

// extractPrefix returns the family prefix from a workload name.
// "tactical-backend" → "tactical"
// "erpnext-redis-cache" → "erpnext"
// "standalone" → "" (no prefix)
func extractPrefix(name string) string {
	idx := strings.Index(name, "-")
	if idx <= 0 || idx >= len(name)-1 {
		return "" // no hyphen or empty prefix/suffix
	}
	prefix := name[:idx]
	if len(prefix) < 3 {
		return "" // too short
	}
	return prefix
}

// augmentServices attaches Services to app groups via strict selector matching.
func augmentServices(ctx context.Context, cs kubernetes.Interface, groups map[AppKey]*appGroup) error {
	svcs, err := cs.CoreV1().Services("").List(ctx, metav1.ListOptions{})
	if err != nil {
		return fmt.Errorf("listing services: %w", err)
	}

	for i := range svcs.Items {
		svc := &svcs.Items[i]
		if len(svc.Spec.Selector) == 0 {
			continue // no-selector services ignored
		}

		// Find matching app group: selector ⊆ pod labels
		for key, g := range groups {
			if key.Namespace != svc.Namespace {
				continue
			}
			if selectorMatches(svc.Spec.Selector, g.podLabels) {
				g.services = append(g.services, *svc)
				break // one service → one group
			}
		}
	}

	return nil
}

// augmentHTTPRoutes attaches HTTPRoute hostnames to app groups.
// Validates: route backendRefs → Service → selector → workload.
func augmentHTTPRoutes(ctx context.Context, gwc gatewayclient.Interface, cs kubernetes.Interface, groups map[AppKey]*appGroup) error {
	routes, err := gwc.GatewayV1().HTTPRoutes("").List(ctx, metav1.ListOptions{})
	if err != nil {
		return fmt.Errorf("listing httproutes: %w", err)
	}

	// Build service→appKey index from already-attached services
	svcToApp := map[string]AppKey{} // "ns/svcname" → AppKey
	for key, g := range groups {
		for _, svc := range g.services {
			svcToApp[svc.Namespace+"/"+svc.Name] = key
		}
	}

	for i := range routes.Items {
		route := &routes.Items[i]
		hosts := extractHosts(route)

		// Resolve backendRefs to services, then to app groups
		for _, rule := range route.Spec.Rules {
			for _, ref := range rule.BackendRefs {
				if ref.Kind != nil && string(*ref.Kind) != "Service" {
					continue
				}
				svcName := string(ref.Name)
				ns := route.Namespace
				if ref.Namespace != nil {
					ns = string(*ref.Namespace)
				}

				appKey, ok := svcToApp[ns+"/"+svcName]
				if !ok {
					continue
				}

				g := groups[appKey]
				gateway := extractGateway(route)
				for _, h := range hosts {
					g.routes = append(g.routes, ExposureRef{
						Kind:    "HTTPRoute",
						Host:    h,
						Name:    route.Name,
						Gateway: gateway,
					})
				}
			}
		}
	}

	return nil
}

// extractHosts returns unique hostnames from an HTTPRoute.
func extractHosts(route *gatewayv1.HTTPRoute) []string {
	seen := map[string]bool{}
	var hosts []string
	for _, h := range route.Spec.Hostnames {
		host := string(h)
		if !seen[host] {
			seen[host] = true
			hosts = append(hosts, host)
		}
	}
	return hosts
}

// extractGateway returns the first gateway parentRef name from an HTTPRoute.
func extractGateway(route *gatewayv1.HTTPRoute) string {
	for _, ref := range route.Spec.ParentRefs {
		if ref.Kind != nil && string(*ref.Kind) != "Gateway" {
			continue
		}
		return string(ref.Name)
	}
	return ""
}

// buildRecords converts app groups into classified AppRecords.
func buildRecords(groups map[AppKey]*appGroup, catalog *Catalog, resolver *CategoryResolver, fluxSources map[AppKey][]DeclaredSource, exposureRules config.ExposureRules) []AppRecord {
	var records []AppRecord

	for _, g := range groups {
		rec := AppRecord{
			Key:      g.identity.Key,
			Category: resolver.Resolve(g.identity.Key.Namespace),
			Collision: g.identity.Key.Identity != g.identity.Key.Identity, // always false; collision set in addWorkload
		}

		// FriendlyName: derive from identity, capitalize
		rec.FriendlyName = titleCase(g.identity.Key.Identity)

		// Components + WorkloadKinds
		kindSet := map[string]bool{}
		for _, wl := range g.workloads {
			rec.Components = append(rec.Components, ComponentRef{Name: wl.name, Kind: wl.kind})
			kindSet[wl.kind] = true
		}
		for k := range kindSet {
			rec.WorkloadKinds = append(rec.WorkloadKinds, k)
		}
		sort.Strings(rec.WorkloadKinds)

		// Images + Version (exclude sidecars and initContainers)
		rec.Images, rec.Version = extractImagesAndVersion(g)

		// Hosts (deduplicated, sorted) + Exposure from gateway parentRefs
		rec.Hosts = dedupeHosts(g.routes)
		rec.Exposure = ClassifyExposure(g.routes, "", "", exposureRules)

		// Replicas + Status
		var totalReady, totalDesired int32
		for _, wl := range g.workloads {
			totalReady += wl.ready
			totalDesired += wl.replicas
		}
		rec.Replicas = fmt.Sprintf("%d/%d", totalReady, totalDesired)
		rec.Status = ComputeStatus(totalReady, totalDesired)

		// Classification (before catalog, so catalog can override)
		rec.Tier = classify(g, rec)

		// Check label override
		for _, wl := range g.workloads {
			if tier, ok := wl.labels[labelTierOverride]; ok {
				rec.Tier = Tier(tier)
				break
			}
		}

		// Attach declared sources from Flux graph
		if srcs, ok := fluxSources[g.identity.Key]; ok {
			rec.Sources = srcs
		}

		// Apply catalog overrides (takes precedence)
		catalog.ApplyOverrides(&rec)

		records = append(records, rec)
	}

	return records
}

// extractImagesAndVersion processes containers, filtering sidecars and initContainers.
func extractImagesAndVersion(g *appGroup) ([]ImageRef, string) {
	seen := map[string]bool{}
	var images []ImageRef
	var tags []string

	for _, wl := range g.workloads {
		// Only main containers (not initContainers)
		for _, c := range wl.containers {
			if IsSidecarImage(c.Image) {
				continue
			}

			ref := parseImage(c.Image)
			key := ref.Repository + ":" + ref.Tag
			if !seen[key] {
				seen[key] = true
				images = append(images, ref)
				if ref.Tag != "" && ref.Tag != "latest" {
					tags = append(tags, ref.Tag)
				}
			}
		}
	}

	// Check version label first
	for _, wl := range g.workloads {
		if v, ok := wl.labels[labelVersion]; ok && v != "" {
			return images, v
		}
	}

	// Tag consensus: only if all non-sidecar tags are identical
	version := ""
	if len(tags) > 0 {
		allSame := true
		for _, t := range tags[1:] {
			if t != tags[0] {
				allSame = false
				break
			}
		}
		if allSame {
			version = tags[0]
		}
	}

	return images, version
}

// parseImage extracts repository and tag from a container image string.
// Handles: repo:tag, repo:tag@digest, repo (no tag → "latest"), malformed.
func parseImage(image string) ImageRef {
	// Strip digest
	if idx := strings.Index(image, "@"); idx > 0 {
		image = image[:idx]
	}

	// Split tag
	// Handle registry:port/repo:tag by finding last colon after last slash
	lastSlash := strings.LastIndex(image, "/")
	colonIdx := strings.LastIndex(image, ":")
	if colonIdx > lastSlash && lastSlash >= 0 {
		return ImageRef{Repository: image[:colonIdx], Tag: image[colonIdx+1:]}
	}
	if colonIdx > 0 && lastSlash < 0 {
		return ImageRef{Repository: image[:colonIdx], Tag: image[colonIdx+1:]}
	}

	return ImageRef{Repository: image, Tag: "latest"}
}

// classify applies heuristic tier classification.
// Default: unsure → platform (deny-by-default for app).
func classify(g *appGroup, rec AppRecord) Tier {
	ns := g.identity.Key.Namespace
	name := strings.ToLower(g.identity.Key.Identity)

	// Platform namespaces
	switch ns {
	case "kube-system", "flux-system", "istio-system", "cert-manager",
		"kube-node-lease", "kube-public":
		return TierPlatform
	}

	// Platform heuristic patterns
	platformPatterns := []string{
		"operator", "controller", "exporter", "agent",
		"ztunnel", "coredns", "cilium", "reflector",
		"kyverno", "alloy", "node-exporter", "webhook",
		"provisioner", "csi-", "snapshot-controller",
	}
	for _, p := range platformPatterns {
		if strings.Contains(name, p) {
			return TierPlatform
		}
	}

	// DaemonSets are usually platform
	allDaemonSet := true
	for _, wl := range g.workloads {
		if wl.kind != "DaemonSet" {
			allDaemonSet = false
			break
		}
	}
	if allDaemonSet {
		return TierPlatform
	}

	// If the app is in a known app namespace and has routes → app
	if len(rec.Hosts) > 0 {
		return TierApp
	}

	// Named app namespaces → app
	appNamespaces := map[string]bool{
		"temple-of-time": true, "swift-sail": true, "lost-woods": true,
		"shooting-gallery": true, "tingle-tuner": true, "hookshot": true,
		"delivery-bag": true, "lens-of-truth": true, "wallmaster": true,
	}
	if appNamespaces[ns] {
		return TierApp
	}

	// Default: unsure → platform
	return TierPlatform
}

// selectorMatches returns true if selector ⊆ labels (strict subset).
func selectorMatches(selector, labels map[string]string) bool {
	if len(selector) == 0 {
		return false
	}
	for k, v := range selector {
		if labels[k] != v {
			return false
		}
	}
	return true
}

// dedupeHosts extracts unique, sorted hostnames from exposure refs.
func dedupeHosts(routes []ExposureRef) []string {
	seen := map[string]bool{}
	var hosts []string
	for _, r := range routes {
		if r.Host != "" && !seen[r.Host] {
			seen[r.Host] = true
			hosts = append(hosts, r.Host)
		}
	}
	sort.Strings(hosts)
	return hosts
}


// resolveFluxSources walks the repo to discover Flux Kustomizations and matches
// them to apps by (namespace + identity). Returns sources keyed by AppKey.
// Uses gitops.DiscoverFluxGraph — local, proven, authoritative.
// Only includes authoritative matches — never guesses.
func resolveFluxSources(repoRoot string, groups map[AppKey]*appGroup) map[AppKey][]DeclaredSource {
	sources := map[AppKey][]DeclaredSource{}

	if repoRoot == "" {
		return sources
	}

	graph, err := gitops.DiscoverFluxGraph(repoRoot)
	if err != nil {
		return sources // can't discover, return empty (not fatal)
	}

	// Build app lookup: namespace → list of app identities
	type appRef struct {
		key      AppKey
		identity string
	}
	appsByNS := map[string][]appRef{}
	for key := range groups {
		appsByNS[key.Namespace] = append(appsByNS[key.Namespace], appRef{key: key, identity: key.Identity})
	}

	// Match Kustomizations to apps by (namespace + identity).
	// Identity match priority:
	//   1. Exact: kustomization.name == app.identity
	//   2. Path segment: last segment of spec.path == app.identity
	//   3. No match → no source (never guess)
	for kKey, kNode := range graph.Kustomizations {
		ns := kKey.Namespace
		apps, ok := appsByNS[ns]
		if !ok {
			continue
		}

		// Try to match this kustomization to an app
		pathSegment := lastPathSegment(kNode.Path)

		for _, app := range apps {
			matched := false
			// Priority 1: exact name match
			if strings.EqualFold(kKey.Name, app.identity) {
				matched = true
			}
			// Priority 2: path segment match
			if !matched && pathSegment != "" && strings.EqualFold(pathSegment, app.identity) {
				matched = true
			}

			if !matched {
				continue
			}

			// Authoritative match — add source
			src := DeclaredSource{
				Kind:     "kustomization",
				RepoPath: kNode.Path,
				Relation: SourceRelationDeploys,
				Primary:  true,
			}

			// Check for duplicates (same app might match multiple kustomizations)
			existing := sources[app.key]
			duplicate := false
			for _, e := range existing {
				if e.RepoPath == src.RepoPath {
					duplicate = true
					break
				}
			}
			if !duplicate {
				// If already has a primary, demote this one
				if len(existing) > 0 {
					src.Primary = false
					// Use shorter path as primary (usually the overlay root)
					if len(src.RepoPath) < len(existing[0].RepoPath) {
						existing[0].Primary = false
						src.Primary = true
					}
				}
				sources[app.key] = append(sources[app.key], src)
			}

			// Resolve dependsOn as secondary sources
			for _, dep := range kNode.DependsOn {
				depNode, ok := graph.Kustomizations[dep]
				if !ok || depNode.Path == "" {
					continue
				}
				depSrc := DeclaredSource{
					Kind:     "kustomization",
					RepoPath: depNode.Path,
					Relation: SourceRelationDependsOn,
				}
				sources[app.key] = append(sources[app.key], depSrc)
			}
		}
	}

	return sources
}

// lastPathSegment returns the last directory segment of a path.
func lastPathSegment(path string) string {
	path = strings.TrimRight(path, "/")
	if idx := strings.LastIndex(path, "/"); idx >= 0 {
		return path[idx+1:]
	}
	return path
}

// sortRecords sorts by category (predefined order) then name (alpha, lowercase).
func sortRecords(records []AppRecord) {
	catOrder := map[string]int{}
	for i, c := range CategoryOrder {
		catOrder[c] = i
	}

	sort.SliceStable(records, func(i, j int) bool {
		ci, oki := catOrder[records[i].Category]
		cj, okj := catOrder[records[j].Category]
		if !oki {
			ci = len(CategoryOrder)
		}
		if !okj {
			cj = len(CategoryOrder)
		}
		if ci != cj {
			return ci < cj
		}
		return strings.ToLower(records[i].FriendlyName) < strings.ToLower(records[j].FriendlyName)
	})
}

// titleCase capitalizes the first letter of each word, handling hyphens.
func titleCase(s string) string {
	words := strings.FieldsFunc(s, func(c rune) bool { return c == '-' || c == '_' })
	for i, w := range words {
		if len(w) > 0 {
			words[i] = strings.ToUpper(w[:1]) + w[1:]
		}
	}
	return strings.Join(words, " ")
}

// ptr32 dereferences an *int32 with a default value.
func ptr32(p *int32, def int32) int32 {
	if p != nil {
		return *p
	}
	return def
}
