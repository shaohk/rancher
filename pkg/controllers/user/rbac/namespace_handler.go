package rbac

import (
	"fmt"
	"strings"
	"time"

	"github.com/pkg/errors"
	"github.com/rancher/norman/types/convert"
	"github.com/rancher/norman/types/slice"
	"github.com/rancher/rancher/pkg/controllers/user/resourcequota"
	namespaceutil "github.com/rancher/rancher/pkg/namespace"
	"github.com/rancher/rancher/pkg/project"
	"github.com/rancher/types/apis/management.cattle.io/v3"
	"github.com/sirupsen/logrus"
	"k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
)

const (
	projectNSGetClusterRoleNameFmt = "%v-namespaces-%v"
	projectNSAnn                   = "authz.cluster.auth.io/project-namespaces"
	initialRoleCondition           = "InitialRolesPopulated"
)

var projectNSVerbToSuffix = map[string]string{
	"get": "readonly",
	"*":   "edit",
}
var defaultProjectLabels = labels.Set(map[string]string{"authz.management.cattle.io/default-project": "true"})
var systemProjectLabels = labels.Set(map[string]string{"authz.management.cattle.io/system-project": "true"})
var initialProjectToLabels = map[string]labels.Set{
	project.Default: defaultProjectLabels,
	project.System:  systemProjectLabels,
}

func newNamespaceLifecycle(m *manager, sync *resourcequota.SyncController) *nsLifecycle {
	return &nsLifecycle{m: m, rq: sync}
}

type nsLifecycle struct {
	m  *manager
	rq *resourcequota.SyncController
}

func (n *nsLifecycle) Create(obj *v1.Namespace) (*v1.Namespace, error) {
	obj, err := n.resourceQuotaInit(obj)
	if err != nil {
		return obj, err
	}

	hasPRTBs, err := n.syncNS(obj)
	if err != nil {
		return obj, err
	}

	if err := n.assignToInitialProject(obj); err != nil {
		return obj, err
	}

	go updateStatusAnnotation(hasPRTBs, *obj, n.m)

	return obj, err
}

func (n *nsLifecycle) resourceQuotaInit(obj *v1.Namespace) (*v1.Namespace, error) {
	return n.rq.CreateResourceQuota(obj)
}

func (n *nsLifecycle) Updated(obj *v1.Namespace) (*v1.Namespace, error) {
	_, err := n.syncNS(obj)
	return obj, err
}

func (n *nsLifecycle) Remove(obj *v1.Namespace) (*v1.Namespace, error) {
	err := n.reconcileNamespaceProjectClusterRole(obj)
	return obj, err
}

func (n *nsLifecycle) syncNS(obj *v1.Namespace) (bool, error) {
	hasPRTBs, err := n.ensurePRTBAddToNamespace(obj)
	if err != nil {
		return false, err
	}

	if err := n.reconcileNamespaceProjectClusterRole(obj); err != nil {
		return false, err
	}

	return hasPRTBs, nil
}

func (n *nsLifecycle) assignToInitialProject(ns *v1.Namespace) error {
	initialProjectsToNamespaces, err := getDefaultAndSystemProjectsToNamespaces()
	if err != nil {
		return err
	}
	for projectName, namespaces := range initialProjectsToNamespaces {
		for _, nsToCheck := range namespaces {
			if nsToCheck == ns.Name {
				projectID := ns.Annotations[projectIDAnnotation]
				if projectID != "" {
					return nil
				}
				projects, err := n.m.projectLister.List(n.m.clusterName, initialProjectToLabels[projectName].AsSelector())
				if err != nil {
					return err
				}
				if len(projects) == 0 {
					continue
				}
				if len(projects) > 1 {
					return fmt.Errorf("cluster [%s] contains more than 1 [%s] project", n.m.clusterName, projectName)
				}
				if projects[0] == nil {
					continue
				}
				if ns.Annotations == nil {
					ns.Annotations = map[string]string{}
				}
				ns.Annotations[projectIDAnnotation] = fmt.Sprintf("%v:%v", n.m.clusterName, projects[0].Name)
			}
		}
	}
	return nil
}

func (n *nsLifecycle) ensurePRTBAddToNamespace(ns *v1.Namespace) (bool, error) {
	// Get project that contain this namespace
	projectID := ns.Annotations[projectIDAnnotation]
	if len(projectID) == 0 {
		rbs, err := n.m.rbLister.List(ns.Name, labels.Everything())
		if err != nil {
			return false, errors.Wrapf(err, "couldn't list role bindings in %s", ns.Name)
		}
		client := n.m.workload.RBAC.RoleBindings(ns.Name).ObjectClient()
		for _, rb := range rbs {
			if uid := convert.ToString(rb.Labels[rtbOwnerLabel]); uid != "" {
				logrus.Infof("Deleting role binding %s in %s", rb.Name, ns.Name)
				if err := client.Delete(rb.Name, &metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
					return false, errors.Wrapf(err, "couldn't delete role binding %s", rb.Name)
				}
			}
		}
		return false, nil
	}

	prtbs, err := n.m.prtbIndexer.ByIndex(prtbByProjectIndex, projectID)
	if err != nil {
		return false, errors.Wrapf(err, "couldn't get project role binding templates associated with project id %s", projectID)
	}
	hasPRTBs := len(prtbs) > 0

	for _, prtb := range prtbs {
		prtb, ok := prtb.(*v3.ProjectRoleTemplateBinding)
		if !ok {
			return false, errors.Wrapf(err, "object %v is not valid project role template binding", prtb)
		}

		if prtb.UserName == "" && prtb.GroupPrincipalName == "" && prtb.GroupName == "" {
			continue
		}

		if prtb.RoleTemplateName == "" {
			logrus.Warnf("ProjectRoleTemplateBinding %v has no role template set. Skipping.", prtb.Name)
			continue
		}

		rt, err := n.m.rtLister.Get("", prtb.RoleTemplateName)
		if err != nil {
			return false, errors.Wrapf(err, "couldn't get role template %v", prtb.RoleTemplateName)
		}

		roles := map[string]*v3.RoleTemplate{}
		if err := n.m.gatherRoles(rt, roles); err != nil {
			return false, err
		}

		if err := n.m.ensureRoles(roles); err != nil {
			return false, errors.Wrap(err, "couldn't ensure roles")
		}

		if err := n.m.ensureProjectRoleBindings(ns.Name, roles, prtb); err != nil {
			return false, errors.Wrapf(err, "couldn't ensure binding %v in %v", prtb.Name, ns.Name)
		}
	}

	var namespace string
	if parts := strings.SplitN(projectID, ":", 2); len(parts) == 2 && len(parts[1]) > 0 {
		namespace = parts[1]
	} else {
		return hasPRTBs, nil
	}

	rbs, err := n.m.rbLister.List(ns.Name, labels.Everything())
	if err != nil {
		return false, errors.Wrapf(err, "couldn't list role bindings in %s", ns.Name)
	}
	client := n.m.workload.RBAC.RoleBindings(ns.Name).ObjectClient()

	for _, rb := range rbs {
		if uid := convert.ToString(rb.Labels[rtbOwnerLabel]); uid != "" {
			prtbs, err := n.m.prtbIndexer.ByIndex(prtbByUIDIndex, uid)
			if err != nil {
				if apierrors.IsNotFound(err) {
					continue
				} else {
					return false, errors.Wrapf(err, "couldn't find prtb for %s", rb.Name)
				}
			}
			for _, prtb := range prtbs {
				if prtb, ok := prtb.(*v3.ProjectRoleTemplateBinding); ok {
					if prtb.Namespace != namespace {
						logrus.Infof("Deleting role binding %s in %s", rb.Name, ns.Name)
						if err := client.Delete(rb.Name, &metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
							return false, errors.Wrapf(err, "couldn't delete role binding %s", rb.Name)
						}
					}
				}
			}
		}
	}

	return hasPRTBs, nil
}

// To ensure that all users in a project can do a GET on the namespaces in that project, this
// function ensures that a ClusterRole exists for the project that grants get access to the
// namespaces in the project. A corresponding PRTB handler will ensure that a binding to this
// ClusterRole exists for every project member
func (n *nsLifecycle) reconcileNamespaceProjectClusterRole(ns *v1.Namespace) error {
	for verb, name := range projectNSVerbToSuffix {
		var desiredRole string
		if ns.DeletionTimestamp == nil {
			if parts := strings.SplitN(ns.Annotations[projectIDAnnotation], ":", 2); len(parts) == 2 && len(parts[1]) > 0 {
				desiredRole = fmt.Sprintf(projectNSGetClusterRoleNameFmt, parts[1], name)
			}
		}

		clusterRoles, err := n.m.crIndexer.ByIndex(crByNSIndex, ns.Name)
		if err != nil {
			return err
		}

		roleCli := n.m.workload.K8sClient.RbacV1().ClusterRoles()
		nsInDesiredRole := false
		for _, c := range clusterRoles {
			cr, ok := c.(*rbacv1.ClusterRole)
			if !ok {
				return errors.Errorf("%v is not a ClusterRole", c)
			}

			if cr.Name == desiredRole {
				nsInDesiredRole = true
				continue
			}

			// This ClusterRole has a reference to the namespace, but is not the desired role. Namespace has been moved; remove it from this ClusterRole
			undesiredRole := cr.DeepCopy()
			modified := false
			for i := range undesiredRole.Rules {
				r := &undesiredRole.Rules[i]
				if slice.ContainsString(r.Verbs, verb) && slice.ContainsString(r.Resources, "namespaces") && slice.ContainsString(r.ResourceNames, ns.Name) {
					modified = true
					resNames := r.ResourceNames
					for i := len(resNames) - 1; i >= 0; i-- {
						if resNames[i] == ns.Name {
							resNames = append(resNames[:i], resNames[i+1:]...)
						}
					}
					r.ResourceNames = resNames
				}
			}
			//if ResourceNames is empty, delete the rule and delete the role if no rules exist
			toDeleteRules := 0
			for _, rule := range undesiredRole.Rules {
				if len(rule.ResourceNames) == 0 {
					toDeleteRules++
				}
			}
			if toDeleteRules == len(undesiredRole.Rules) {
				logrus.Infof("Deleting ClusterRole %s", undesiredRole.Name)
				if err = roleCli.Delete(undesiredRole.Name, &metav1.DeleteOptions{}); err != nil {
					return err
				}
				continue
			} else if toDeleteRules != 0 {
				var updatedRules []rbacv1.PolicyRule
				for _, rule := range undesiredRole.Rules {
					if len(rule.ResourceNames) != 0 {
						updatedRules = append(updatedRules, rule)
					}
				}
				undesiredRole.Rules = updatedRules
			}
			if modified {
				if _, err = roleCli.Update(undesiredRole); err != nil {
					return err
				}
			}
		}

		if !nsInDesiredRole && desiredRole != "" {
			mustUpdate := true
			cr, err := n.m.crLister.Get("", desiredRole)
			if err != nil && !apierrors.IsNotFound(err) {
				return err
			}

			// Create new role
			if cr == nil {
				return n.m.createProjectNSRole(desiredRole, verb, ns.Name)
			}

			// Check to see if retrieved role has the namespace (small chance cache could have been updated)
			for _, r := range cr.Rules {
				if slice.ContainsString(r.Verbs, verb) && slice.ContainsString(r.Resources, "namespaces") && slice.ContainsString(r.ResourceNames, ns.Name) {
					// ns already in the role, nothing to do
					mustUpdate = false
				}
			}
			if mustUpdate {
				cr = cr.DeepCopy()
				appendedToExisting := false
				for i := range cr.Rules {
					r := &cr.Rules[i]
					if slice.ContainsString(r.Verbs, verb) && slice.ContainsString(r.Resources, "namespaces") {
						r.ResourceNames = append(r.ResourceNames, ns.Name)
						appendedToExisting = true
						break
					}
				}

				if !appendedToExisting {
					cr.Rules = append(cr.Rules, rbacv1.PolicyRule{
						APIGroups:     []string{""},
						Verbs:         []string{verb},
						Resources:     []string{"namespaces"},
						ResourceNames: []string{ns.Name},
					})
				}

				_, err = roleCli.Update(cr)
				return err
			}
		}
	}

	return nil
}

func (m *manager) createProjectNSRole(roleName, verb, ns string) error {
	roleCli := m.workload.K8sClient.RbacV1().ClusterRoles()

	cr := &rbacv1.ClusterRole{
		ObjectMeta: metav1.ObjectMeta{
			Name:        roleName,
			Annotations: map[string]string{projectNSAnn: roleName},
		},
	}
	if ns != "" {
		cr.Rules = []rbacv1.PolicyRule{
			{
				APIGroups:     []string{""},
				Verbs:         []string{verb},
				Resources:     []string{"namespaces"},
				ResourceNames: []string{ns},
			},
		}
	}
	_, err := roleCli.Create(cr)
	return err
}

func crByNS(obj interface{}) ([]string, error) {
	cr, ok := obj.(*rbacv1.ClusterRole)
	if !ok {
		return []string{}, nil
	}

	if _, ok := cr.Annotations[projectNSAnn]; !ok {
		return []string{}, nil
	}

	var result []string
	for _, r := range cr.Rules {
		if slice.ContainsString(r.Resources, "namespaces") && (slice.ContainsString(r.Verbs, "get") || slice.ContainsString(r.Verbs, "*")) {
			result = append(result, r.ResourceNames...)
		}
	}
	return result, nil
}

func updateStatusAnnotation(hasPRTBs bool, namespace v1.Namespace, mgr *manager) {
	if _, ok := namespace.Annotations[projectIDAnnotation]; ok {
		for i := 0; i < 10; i++ {
			time.Sleep(time.Millisecond * 500)
			clusterRoles, err := mgr.crIndexer.ByIndex(crByNSIndex, namespace.Name)
			if err != nil {
				logrus.Warnf("error getting cluster roles for ns %v for status update: %v", namespace.Name, err)
				continue
			}
			if len(clusterRoles) < 2 {
				continue
			}

			creator := namespace.Annotations["field.cattle.io/creatorId"]
			if creator != "" {
				found := false
				for _, crx := range clusterRoles {
					cr, _ := crx.(*rbacv1.ClusterRole)
					crbKey := rbRoleSubjectKey(cr.Name, rbacv1.Subject{Kind: "User", Name: creator})
					crbs, _ := mgr.crbIndexer.ByIndex(crbByRoleAndSubjectIndex, crbKey)
					if len(crbs) > 0 {
						found = true
						break
					}
				}
				if !found {
					continue
				}
			}

			if hasPRTBs {
				bindings, err := mgr.rbLister.List(namespace.Name, labels.Everything())
				if err != nil {
					logrus.Warnf("error getting bindings for ns %v for status update: %v", namespace.Name, err)
					continue
				}
				if len(bindings) > 0 {
					break
				}
			}
		}
	}

	ns, err := mgr.workload.Core.Namespaces("").Get(namespace.Name, metav1.GetOptions{})
	if err != nil {
		logrus.Errorf("error getting ns %v for status update: %v", namespace.Name, err)
		return
	}
	namespaceutil.SetNamespaceCondition(ns, time.Second*1, initialRoleCondition, true, "")
	_, err = mgr.workload.Core.Namespaces("").Update(ns)
	if err != nil {
		logrus.Errorf("error updating ns %v status: %v", ns.Name, err)
	}
}
