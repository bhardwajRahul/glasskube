package web

import (
	"context"
	"embed"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"

	"github.com/glasskube/glasskube/internal/dependency/graph"
	"github.com/glasskube/glasskube/internal/telemetry/annotations"

	"github.com/glasskube/glasskube/internal/web/components/toast"

	"github.com/glasskube/glasskube/internal/web/sse"
	"github.com/glasskube/glasskube/internal/web/sse/refresh"

	clientadapter "github.com/glasskube/glasskube/internal/adapter/goclient"

	"github.com/glasskube/glasskube/internal/controller/ctrlpkg"

	"github.com/glasskube/glasskube/internal/web/util"

	"github.com/Masterminds/semver/v3"
	"github.com/glasskube/glasskube/api/v1alpha1"
	"github.com/glasskube/glasskube/internal/clientutils"
	"github.com/glasskube/glasskube/internal/cliutils"
	"github.com/glasskube/glasskube/internal/config"
	"github.com/glasskube/glasskube/internal/dependency"
	"github.com/glasskube/glasskube/internal/manifestvalues"
	repoclient "github.com/glasskube/glasskube/internal/repo/client"
	repotypes "github.com/glasskube/glasskube/internal/repo/types"
	"github.com/glasskube/glasskube/internal/telemetry"
	"github.com/glasskube/glasskube/internal/web/handler"
	"github.com/glasskube/glasskube/pkg/bootstrap"
	"github.com/glasskube/glasskube/pkg/client"
	"github.com/glasskube/glasskube/pkg/list"
	"github.com/glasskube/glasskube/pkg/open"
	"github.com/glasskube/glasskube/pkg/uninstall"
	"github.com/glasskube/glasskube/pkg/update"
	"github.com/gorilla/mux"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	corev1 "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/tools/clientcmd/api"
	"k8s.io/klog/v2"
)

//go:embed root
//go:embed templates
var embeddedFs embed.FS
var webFs fs.FS = embeddedFs

func init() {
	if config.IsDevBuild() {
		if _, err := os.Lstat(templatesBaseDir); err == nil {
			webFs = os.DirFS(templatesBaseDir)
		}
	}
}

type ServerOptions struct {
	Host               string
	Port               string
	Kubeconfig         string
	LogLevel           int
	SkipOpeningBrowser bool
}

func NewServer(options ServerOptions) *server {
	server := server{
		ServerOptions:           options,
		configLoader:            &defaultConfigLoader{options.Kubeconfig},
		forwarders:              make(map[string]*open.OpenResult),
		templates:               templates{},
		stopCh:                  make(chan struct{}, 1),
		httpServerHasShutdownCh: make(chan struct{}, 1),
	}
	return &server
}

type server struct {
	ServerOptions
	configLoader
	listener                net.Listener
	restConfig              *rest.Config
	rawConfig               *api.Config
	pkgClient               client.PackageV1Alpha1Client
	nonCachedClient         client.PackageV1Alpha1Client
	repoClientset           repoclient.RepoClientset
	k8sClient               *kubernetes.Clientset
	broadcaster             *sse.Broadcaster
	namespaceLister         *corev1.NamespaceLister
	configMapLister         *corev1.ConfigMapLister
	secretLister            *corev1.SecretLister
	forwarders              map[string]*open.OpenResult
	forwardersMutex         sync.Mutex
	dependencyMgr           *dependency.DependendcyManager
	valueResolver           *manifestvalues.Resolver
	isBootstrapped          bool
	templates               templates
	httpServer              *http.Server
	httpServerHasShutdownCh chan struct{}
	stopCh                  chan struct{}
}

func (s *server) RestConfig() *rest.Config {
	return s.restConfig
}

func (s *server) RawConfig() *api.Config {
	return s.rawConfig
}

func (s *server) Client() client.PackageV1Alpha1Client {
	return s.pkgClient
}

func (s *server) K8sClient() *kubernetes.Clientset {
	return s.k8sClient
}

func (s *server) RepoClient() repoclient.RepoClientset {
	return s.repoClientset
}

func initLogging(level int) {
	klog.InitFlags(nil)
	_ = flag.Set("v", strconv.Itoa(level))
	flag.Parse()
}

func (s *server) Start(ctx context.Context) error {
	if s.listener != nil {
		return errors.New("server is already listening")
	}

	if s.LogLevel != 0 {
		initLogging(s.LogLevel)
	} else if config.IsDevBuild() {
		initLogging(5)
	}

	s.templates.parseTemplates()
	if config.IsDevBuild() {
		if err := s.templates.watchTemplates(); err != nil {
			fmt.Fprintf(os.Stderr, "templates will not be parsed after changes: %v\n", err)
		}
	}
	s.broadcaster = sse.NewBroadcaster()
	_ = s.ensureBootstrapped(ctx)

	root, err := fs.Sub(webFs, "root")
	if err != nil {
		return err
	}

	fileServer := http.FileServer(http.FS(root))

	router := mux.NewRouter()
	router.Use(telemetry.HttpMiddleware(telemetry.WithPathRedactor(packagesPathRedactor)))
	router.PathPrefix("/static/").Handler(fileServer)
	router.Handle("/favicon.ico", fileServer)
	router.HandleFunc("/events", s.broadcaster.Handler)
	router.HandleFunc("/support", s.supportPage)
	router.HandleFunc("/kubeconfig", s.kubeconfigPage)
	router.Handle("/bootstrap", s.requireKubeconfig(s.bootstrapPage))
	router.Handle("/kubeconfig/persist", s.requireKubeconfig(s.persistKubeconfig))
	// overview pages
	router.Handle("/packages", s.requireReady(s.packages))
	router.Handle("/clusterpackages", s.requireReady(s.clusterPackages))

	// detail page endpoints
	pkgBasePath := "/packages/{manifestName}"
	installedPkgBasePath := pkgBasePath + "/{namespace}/{name}"
	clpkgBasePath := "/clusterpackages/{pkgName}"
	router.Handle(pkgBasePath, s.requireReady(s.packageDetail))
	router.Handle(installedPkgBasePath, s.requireReady(s.packageDetail))
	router.Handle(clpkgBasePath, s.requireReady(s.clusterPackageDetail))
	// discussion endpoints
	router.Handle(pkgBasePath+"/discussion", s.requireReady(s.packageDiscussion))
	router.Handle(installedPkgBasePath+"/discussion", s.requireReady(s.packageDiscussion))
	router.Handle(clpkgBasePath+"/discussion", s.requireReady(s.clusterPackageDiscussion))
	router.Handle(pkgBasePath+"/discussion/badge", s.requireReady(s.discussionBadge))
	router.Handle(installedPkgBasePath+"/discussion/badge", s.requireReady(s.discussionBadge))
	router.Handle(clpkgBasePath+"/discussion/badge", s.requireReady(s.discussionBadge))
	// configuration endpoints
	router.Handle(pkgBasePath+"/configuration/{valueName}", s.requireReady(s.packageConfigurationInput))
	router.Handle(installedPkgBasePath+"/configuration/{valueName}", s.requireReady(s.packageConfigurationInput))
	router.Handle(clpkgBasePath+"/configuration/{valueName}", s.requireReady(s.clusterPackageConfigurationInput))
	// open endpoints
	router.Handle(installedPkgBasePath+"/open", s.requireReady(s.open))
	router.Handle(clpkgBasePath+"/open", s.requireReady(s.open))
	// uninstall endpoints
	router.Handle(installedPkgBasePath+"/uninstall", s.requireReady(s.uninstall))
	router.Handle(clpkgBasePath+"/uninstall", s.requireReady(s.uninstall))
	// suspend endpoints
	router.Handle(clpkgBasePath+"/suspend", s.requireReady(s.handleSuspend))
	router.Handle(clpkgBasePath+"/resume", s.requireReady(s.handleResume))
	router.Handle(installedPkgBasePath+"/suspend", s.requireReady(s.handleSuspend))
	router.Handle(installedPkgBasePath+"/resume", s.requireReady(s.handleResume))

	// configuration datalist endpoints
	router.Handle("/datalists/{valueName}/names", s.requireReady(s.namesDatalist))
	router.Handle("/datalists/{valueName}/keys", s.requireReady(s.keysDatalist))
	// settings
	router.Handle("/settings", s.requireReady(s.settingsPage))
	router.Handle("/settings/repository/{repoName}", s.requireReady(s.repositoryConfig))
	router.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/clusterpackages", http.StatusFound)
	})
	http.Handle("/", s.enrichContext(router))

	s.listener, err = net.Listen("tcp", net.JoinHostPort(s.Host, s.Port))
	if err != nil {
		// if the error is "address already in use", try to get the OS to assign a random free port
		if errors.Is(err, syscall.EADDRINUSE) {
			fmt.Fprintf(os.Stderr, "could not start server: %v\n", err)
			if cliutils.YesNoPrompt("Should glasskube try to use a different (random) port?", true) {
				s.listener, err = net.Listen("tcp", net.JoinHostPort(s.Host, "0"))
				if err != nil {
					return err
				}
			} else {
				return err
			}
		} else {
			return err
		}
	}

	browseUrl := fmt.Sprintf("http://%s", s.listener.Addr())
	fmt.Fprintln(os.Stderr, "glasskube UI is available at", browseUrl)
	if !s.SkipOpeningBrowser {
		_ = cliutils.OpenInBrowser(browseUrl)
	}

	go s.broadcaster.Run(s.stopCh)
	s.httpServer = &http.Server{}

	var receivedSig *os.Signal
	go func() {
		sigint := make(chan os.Signal, 1)
		signal.Notify(sigint, syscall.SIGTERM, syscall.SIGINT)
		sig := <-sigint
		receivedSig = &sig
		s.shutdown()
	}()

	err = s.httpServer.Serve(s.listener)
	if err != nil && err != http.ErrServerClosed {
		return err
	}

	<-s.httpServerHasShutdownCh
	cliutils.ExitFromSignal(receivedSig)

	return nil
}

func (s *server) shutdown() {
	close(s.stopCh)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := s.httpServer.Shutdown(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "Failed to shutdown server: %v\n", err)
	}
	close(s.httpServerHasShutdownCh)
}

// uninstall is an endpoint, which returns the modal html for GET requests, and performs the uninstallation for POST
func (s *server) uninstall(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	pkgName := mux.Vars(r)["pkgName"]
	manifestName := mux.Vars(r)["manifestName"]
	namespace := mux.Vars(r)["namespace"]
	name := mux.Vars(r)["name"]

	if r.Method == http.MethodPost {
		uninstaller := uninstall.NewUninstaller(s.pkgClient)
		if pkgName != "" {
			var pkg v1alpha1.ClusterPackage
			if err := s.pkgClient.ClusterPackages().Get(ctx, pkgName, &pkg); err != nil {
				s.sendToast(w, toast.WithErr(fmt.Errorf("failed to fetch clusterpackage %v: %w", pkgName, err)))
				return
			}
			if err := uninstaller.Uninstall(ctx, &pkg, false); err != nil {
				s.sendToast(w, toast.WithErr(fmt.Errorf("failed to uninstall clusterpackage %v: %w", pkgName, err)))
				return
			}
		} else {
			var pkg v1alpha1.Package
			if err := s.pkgClient.Packages(namespace).Get(ctx, name, &pkg); err != nil {
				s.sendToast(w, toast.WithErr(fmt.Errorf("failed to fetch package %v/%v: %w", namespace, name, err)))
				return
			}
			if err := uninstaller.Uninstall(ctx, &pkg, false); err != nil {
				s.sendToast(w, toast.WithErr(fmt.Errorf("failed to uninstall package %v/%v: %w", namespace, name, err)))
				return
			}
		}
	} else {
		if pkgName != "" {
			var pruned []graph.PackageRef
			var err error
			if g, err1 := s.dependencyMgr.NewGraph(r.Context()); err1 != nil {
				err = fmt.Errorf("error validating uninstall: %w", err1)
			} else {
				g.Delete(pkgName, "")
				pruned = g.Prune()
				if err1 := g.Validate(); err1 != nil {
					err = fmt.Errorf("%v cannot be uninstalled: %w", pkgName, err1)
				}
			}
			err = s.templates.pkgUninstallModalTmpl.Execute(w, map[string]any{
				"PackageName": pkgName,
				"Pruned":      pruned,
				"Err":         err,
				"PackageHref": util.GetClusterPkgHref(pkgName),
				"GitopsMode":  s.isGitopsModeEnabled(),
			})
			util.CheckTmplError(err, "pkgUninstallModalTmpl")
		} else {
			var pruned []graph.PackageRef
			var err error
			// TODO: refactor this duplicate code segment
			if g, err1 := s.dependencyMgr.NewGraph(r.Context()); err1 != nil {
				err = fmt.Errorf("error validating uninstall: %w", err1)
			} else {
				g.Delete(name, namespace)
				pruned = g.Prune()
				if err1 := g.Validate(); err1 != nil {
					err = fmt.Errorf("%v cannot be uninstalled: %w", pkgName, err1)
				}
			}
			err = s.templates.pkgUninstallModalTmpl.Execute(w, map[string]any{
				"Namespace":   namespace,
				"Name":        name,
				"Pruned":      pruned,
				"Err":         err,
				"PackageHref": util.GetNamespacedPkgHref(manifestName, namespace, name),
				"GitopsMode":  s.isGitopsModeEnabled(),
			})
			util.CheckTmplError(err, "pkgUninstallModalTmpl")
		}
	}
}

func (s *server) open(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	pkgName := mux.Vars(r)["pkgName"]
	namespace := mux.Vars(r)["namespace"]
	name := mux.Vars(r)["name"]

	if pkgName != "" {
		var pkg v1alpha1.ClusterPackage
		if err := s.pkgClient.ClusterPackages().Get(ctx, pkgName, &pkg); err != nil {
			s.sendToast(w, toast.WithErr(fmt.Errorf("failed to fetch clusterpackage %v: %w", pkgName, err)))
			return
		}
		s.handleOpen(ctx, w, &pkg)
	} else {
		var pkg v1alpha1.Package
		if err := s.pkgClient.Packages(namespace).Get(ctx, name, &pkg); err != nil {
			s.sendToast(w, toast.WithErr(fmt.Errorf("failed to fetch package %v/%v: %w", namespace, name, err)))
			return
		}
		s.handleOpen(ctx, w, &pkg)
	}
}

func (s *server) handleOpen(ctx context.Context, w http.ResponseWriter, pkg ctrlpkg.Package) {
	fwName := cache.NewObjectName(pkg.GetNamespace(), pkg.GetName()).String()
	s.forwardersMutex.Lock()
	defer s.forwardersMutex.Unlock()
	if result, ok := s.forwarders[fwName]; ok {
		result.WaitReady()
		_ = cliutils.OpenInBrowser(result.Url)
		return
	}

	result, err := open.NewOpener().Open(ctx, pkg, "", s.Host, 0)
	if err != nil {
		s.sendToast(w, toast.WithErr(fmt.Errorf("failed to open %v: %w", pkg.GetName(), err)))
	} else {
		s.forwarders[fwName] = result
		result.WaitReady()
		_ = cliutils.OpenInBrowser(result.Url)
		w.WriteHeader(http.StatusAccepted)

		go func() {
			ctx = context.WithoutCancel(ctx)
		resultLoop:
			for {
				select {
				case <-s.stopCh:
					break resultLoop
				case fwErr := <-result.Completion:
					// note: this does not happen in "realtime" (e.g. when the forwarded-to-pod is deleted), but only
					// the next time a connection on that port is requested, e.g. when the user reloads the forwarded
					// page or clicks open again – only then we will end up here.
					if fwErr != nil {
						fmt.Fprintf(os.Stderr, "forwarder %v completed with error: %v\n", fwName, fwErr)
					} else {
						fmt.Fprintf(os.Stderr, "forwarder %v completed without error\n", fwName)
					}
					// try to re-open if the package is still installed
					if pkg.IsNamespaceScoped() {
						var p v1alpha1.Package
						if err := s.pkgClient.Packages(pkg.GetNamespace()).Get(ctx, pkg.GetName(), &p); err != nil {
							if !apierrors.IsNotFound(err) {
								fmt.Fprintf(os.Stderr, "can not reopen %v: %v\n", fwName, err)
							}
							break resultLoop
						} else {
							pkg = &p
						}
					} else {
						var cp v1alpha1.ClusterPackage
						if err := s.pkgClient.ClusterPackages().Get(ctx, pkg.GetName(), &cp); err != nil {
							if !apierrors.IsNotFound(err) {
								fmt.Fprintf(os.Stderr, "can not reopen %v: %v\n", fwName, err)
							}
							break resultLoop
						} else {
							pkg = &cp
						}
					}
					result, err = open.NewOpener().Open(ctx, pkg, "", s.Host, 0)
					s.forwardersMutex.Lock()
					if err != nil {
						fmt.Fprintf(os.Stderr, "failed to reopen forwarder %v: %v\n", fwName, err)
						delete(s.forwarders, fwName)
						s.forwardersMutex.Unlock()
						break resultLoop
					} else {
						fmt.Fprintf(os.Stderr, "reopened forwarder %v\n", fwName)
						s.forwarders[fwName] = result
						s.forwardersMutex.Unlock()
					}
				}
			}
		}()
	}
}

func (s *server) clusterPackages(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	clpkgs, listErr := list.NewLister(ctx).GetClusterPackagesWithStatus(ctx, list.ListOptions{IncludePackageInfos: true})
	if listErr != nil && len(clpkgs) == 0 {
		listErr = fmt.Errorf("could not load clusterpackages: %w", listErr)
		fmt.Fprintf(os.Stderr, "%v\n", listErr)
	}

	// Call isUpdateAvailable for each installed clusterpackage.
	// This is not the same as getting all updates in a single transaction, because some dependency
	// conflicts could be resolvable by installing individual clpkgs.
	installedClpkgs := make([]ctrlpkg.Package, 0, len(clpkgs))
	clpkgUpdateAvailable := map[string]bool{}
	for _, pkg := range clpkgs {
		if pkg.ClusterPackage != nil {
			installedClpkgs = append(installedClpkgs, pkg.ClusterPackage)
		}
		clpkgUpdateAvailable[pkg.Name] = s.isUpdateAvailableForPkg(r.Context(), pkg.ClusterPackage)
	}

	overallUpdatesAvailable := false
	if len(installedClpkgs) > 0 {
		overallUpdatesAvailable = s.isUpdateAvailable(r.Context(), installedClpkgs)
	}

	tmplErr := s.templates.clusterPkgsPageTemplate.Execute(w, s.enrichPage(r, map[string]any{
		"ClusterPackages":               clpkgs,
		"ClusterPackageUpdateAvailable": clpkgUpdateAvailable,
		"UpdatesAvailable":              overallUpdatesAvailable,
		"PackageHref":                   util.GetClusterPkgHref("-"),
	}, listErr))
	util.CheckTmplError(tmplErr, "clusterpackages")
}

func (s *server) packages(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	allPkgs, listErr := list.NewLister(ctx).GetPackagesWithStatus(ctx, list.ListOptions{IncludePackageInfos: true})
	if listErr != nil {
		listErr = fmt.Errorf("could not load packages: %w", listErr)
		fmt.Fprintf(os.Stderr, "%v\n", listErr)
		// TODO check again
	}

	packageUpdateAvailable := map[string]bool{}
	var installed []*list.PackagesWithStatus
	var available []*repotypes.PackageRepoIndexItem
	var installedPkgs []ctrlpkg.Package
	for _, pkgsWithStatus := range allPkgs {
		if len(pkgsWithStatus.Packages) > 0 {
			for _, pkgWithStatus := range pkgsWithStatus.Packages {
				installedPkgs = append(installedPkgs, pkgWithStatus.Package)

				// Call isUpdateAvailable for each installed package.
				// This is not the same as getting all updates in a single transaction, because some dependency
				// conflicts could be resolvable by installing individual packages.
				packageUpdateAvailable[cache.MetaObjectToName(pkgWithStatus.Package).String()] =
					s.isUpdateAvailableForPkg(ctx, pkgWithStatus.Package)
			}
			installed = append(installed, pkgsWithStatus)
		} else {
			available = append(available, &pkgsWithStatus.PackageRepoIndexItem)
		}
	}

	overallUpdatesAvailable := false
	if len(installedPkgs) > 0 {
		overallUpdatesAvailable = s.isUpdateAvailable(r.Context(), installedPkgs)
	}

	tmplErr := s.templates.pkgsPageTmpl.Execute(w, s.enrichPage(r, map[string]any{
		"InstalledPackages":      installed,
		"AvailablePackages":      available,
		"PackageUpdateAvailable": packageUpdateAvailable,
		"UpdatesAvailable":       overallUpdatesAvailable,
		"PackageHref":            util.GetNamespacedPkgHref("-", "-", "-"),
	}, listErr))
	util.CheckTmplError(tmplErr, "packages")
}

func (s *server) isGitopsModeEnabled() bool {
	if ns, err := (*s.namespaceLister).Get("glasskube-system"); err != nil {
		fmt.Fprintf(os.Stderr, "failed to fetch glasskube-system namespace: %v\n", err)
		return true
	} else {
		return annotations.IsGitopsModeEnabled(ns.Annotations)
	}
}

func (s *server) supportPage(w http.ResponseWriter, r *http.Request) {
	if err := s.ensureBootstrapped(r.Context()); err != nil {
		if err.BootstrapMissing() {
			http.Redirect(w, r, "/bootstrap", http.StatusFound)
			return
		}
		err := s.templates.supportPageTmpl.Execute(w, &map[string]any{
			"CurrentContext":            "",
			"KubeconfigDefaultLocation": clientcmd.RecommendedHomeFile,
			"Err":                       err,
		})
		util.CheckTmplError(err, "support")
	} else {
		http.Redirect(w, r, "/", http.StatusFound)
	}
}

func (s *server) bootstrapPage(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if r.Method == "POST" {
		client := bootstrap.NewBootstrapClient(s.restConfig)
		if _, err := client.Bootstrap(ctx, bootstrap.DefaultOptions()); err != nil {
			fmt.Fprintf(os.Stderr, "\nAn error occurred during bootstrap:\n%v\n", err)
			err := s.templates.bootstrapPageTmpl.ExecuteTemplate(w, "bootstrap-failure", nil)
			util.CheckTmplError(err, "bootstrap-failure")
		} else {
			err := s.templates.bootstrapPageTmpl.ExecuteTemplate(w, "bootstrap-success", nil)
			util.CheckTmplError(err, "bootstrap-success")
		}
	} else {
		isBootstrapped, err := bootstrap.IsBootstrapped(ctx, s.restConfig)
		if err != nil {
			fmt.Fprintf(os.Stderr, "\nFailed to check whether Glasskube is bootstrapped: %v\n\n", err)
		} else if isBootstrapped {
			http.Redirect(w, r, "/", http.StatusFound)
			return
		}
		tplErr := s.templates.bootstrapPageTmpl.Execute(w, &map[string]any{
			"CloudId":        telemetry.GetMachineId(),
			"CurrentContext": s.rawConfig.CurrentContext,
			"Err":            err,
		})
		util.CheckTmplError(tplErr, "bootstrap")
	}
}

func (s *server) kubeconfigPage(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPost {
		file, _, err := r.FormFile("kubeconfig")
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		data, err := io.ReadAll(file)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		s.loadBytesConfig(data)
		if err := s.checkKubeconfig(); err != nil {
			fmt.Fprintf(os.Stderr, "The selected kubeconfig is invalid: %v\n", err)
		} else {
			fmt.Fprintln(os.Stderr, "The selected kubeconfig is valid!")
		}
	}

	configErr := s.checkKubeconfig()
	var currentContext string
	if s.rawConfig != nil {
		currentContext = s.rawConfig.CurrentContext
	}
	tplErr := s.templates.kubeconfigPageTmpl.Execute(w, map[string]any{
		"CloudId":                   telemetry.GetMachineId(),
		"CurrentContext":            currentContext,
		"ConfigErr":                 configErr,
		"KubeconfigDefaultLocation": clientcmd.RecommendedHomeFile,
		"DefaultKubeconfigExists":   defaultKubeconfigExists(),
	})
	util.CheckTmplError(tplErr, "kubeconfig")
}

func (s *server) settingsPage(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPost {
		formVal := r.FormValue(advancedOptionsKey)
		setAdvancedOptionsCookie(w, formVal == "on")
	} else if r.Method == http.MethodGet {
		var repos v1alpha1.PackageRepositoryList
		if err := s.pkgClient.PackageRepositories().GetAll(r.Context(), &repos); err != nil {
			s.sendToast(w, toast.WithErr(fmt.Errorf("failed to fetch repositories: %w", err)))
			return
		}

		advancedOptions, err := getAdvancedOptionsFromCookie(r)
		if err != nil {
			fmt.Fprintf(os.Stderr, "failed to get advanced options from cookie: %v\n", err)
		}
		tmplErr := s.templates.settingsPageTmpl.Execute(w, s.enrichPage(r, map[string]any{
			"Repositories":    repos.Items,
			"AdvancedOptions": advancedOptions,
		}, nil))
		util.CheckTmplError(tmplErr, "settings")
	}
}

func (s *server) repositoryConfig(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		s.getHandleRepositoryConfig(w, r)
	case http.MethodPost:
		s.getUpdateRepositoryConfig(w, r)
	}

}

func (s *server) getHandleRepositoryConfig(w http.ResponseWriter, r *http.Request) {
	repoName := mux.Vars(r)["repoName"]
	var repo v1alpha1.PackageRepository
	if err := s.pkgClient.PackageRepositories().Get(r.Context(), repoName, &repo); err != nil {
		// error handling
		s.sendToast(w, toast.WithErr(fmt.Errorf("failed to fetch repositories: %w", err)))
		return
	}
	tmplErr := s.templates.repositoryPageTmpl.Execute(w, s.enrichPage(r, map[string]any{
		"Repository": repo,
	}, nil))
	util.CheckTmplError(tmplErr, "repository")

}

func (s *server) getUpdateRepositoryConfig(w http.ResponseWriter, r *http.Request) {
	repoName := mux.Vars(r)["repoName"]
	repoUrl := r.FormValue("url")
	checkDefault := r.FormValue("default")
	opts := metav1.UpdateOptions{}
	var repo v1alpha1.PackageRepository
	var defaultRepo *v1alpha1.PackageRepository
	var err error

	if err := s.pkgClient.PackageRepositories().Get(r.Context(), repoName, &repo); err != nil {
		s.sendToast(w, toast.WithErr(fmt.Errorf("failed to fetch repositories: %w", err)))
		return
	}

	if repoUrl != "" {
		if _, err := url.ParseRequestURI(repoUrl); err != nil {
			s.sendToast(w, toast.WithErr(fmt.Errorf("use a valid URL for the package repository (got %v)", err)))
			return
		}
		repo.Spec.Url = repoUrl
	}

	repo.Spec.Auth = nil

	if checkDefault == "on" {
		defaultRepo, err = cliutils.GetDefaultRepo(r.Context())
		if errors.Is(err, cliutils.NoDefaultRepo) {
			repo.SetDefaultRepository()
		} else if err != nil {
			s.sendToast(w, toast.WithErr(fmt.Errorf("failed to fetch repositories: %w", err)))
			return
		} else if defaultRepo.Name != repoName {
			defaultRepo.SetDefaultRepositoryBool(false)
			if err := s.pkgClient.PackageRepositories().Update(r.Context(), defaultRepo, opts); err != nil {
				s.sendToast(w, toast.WithErr(fmt.Errorf(" error updating current default package repository: %v", err)))
				return
			}
			repo.SetDefaultRepository()
		}
	}

	if err := s.pkgClient.PackageRepositories().Update(r.Context(), &repo, opts); err != nil {
		s.sendToast(w, toast.WithErr(fmt.Errorf(" error updating the package repository: %v", err)))
		if checkDefault == "on" && defaultRepo != nil && defaultRepo.Name != repoName {
			defaultRepo.SetDefaultRepositoryBool(true)
			if err := s.pkgClient.PackageRepositories().Update(r.Context(), defaultRepo, opts); err != nil {
				s.sendToast(w, toast.WithErr(fmt.Errorf(" error rolling back to default package repository: %v", err)))
			}
		}
		return
	}
	s.swappingRedirect(w, "/settings", "main", "main")
}

func (s *server) enrichPage(r *http.Request, data map[string]any, err error) map[string]any {
	data["CloudId"] = telemetry.GetMachineId()
	if pathParts := strings.Split(r.URL.Path, "/"); len(pathParts) >= 2 {
		data["NavbarActiveItem"] = pathParts[1]
	}
	data["Error"] = err
	data["CurrentContext"] = s.rawConfig.CurrentContext
	data["GitopsMode"] = s.isGitopsModeEnabled()
	operatorVersion, clientVersion, err := s.getGlasskubeVersions(r.Context())
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to check for version mismatch: %v\n", err)
	} else if operatorVersion != nil && clientVersion != nil && !operatorVersion.Equal(clientVersion) {
		data["VersionMismatchWarning"] = true
	}
	if operatorVersion != nil && clientVersion != nil && !config.IsDevBuild() {
		data["VersionDetails"] = map[string]any{
			"OperatorVersion":     operatorVersion.String(),
			"ClientVersion":       clientVersion.String(),
			"NeedsOperatorUpdate": operatorVersion.LessThan(clientVersion),
			"GitopsMode":          s.isGitopsModeEnabled(),
		}
	}
	if config.IsDevBuild() {
		data["VersionDetails"] = map[string]any{
			"OperatorVersion": config.Version,
			"ClientVersion":   config.Version,
		}
	}
	data["CacheBustingString"] = config.Version
	return data
}

func (server *server) getGlasskubeVersions(ctx context.Context) (*semver.Version, *semver.Version, error) {
	if !config.IsDevBuild() {
		if operatorVersion, err := clientutils.GetPackageOperatorVersion(ctx); err != nil {
			fmt.Fprintf(os.Stderr, "Failed to check package operator version: %v\n", err)
			return nil, nil, err
		} else if parsedOperator, err := semver.NewVersion(operatorVersion); err != nil {
			fmt.Fprintf(os.Stderr, "Failed to parse operator version %v: %v\n", operatorVersion, err)
			return nil, nil, err
		} else if parsedClient, err := semver.NewVersion(config.Version); err != nil {
			fmt.Fprintf(os.Stderr, "Failed to parse client version %v: %v\n", config.Version, err)
			return nil, nil, err
		} else {
			return parsedOperator, parsedClient, nil
		}
	}
	return nil, nil, nil
}

func (s *server) persistKubeconfig(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPost {
		if !defaultKubeconfigExists() {
			if err := clientcmd.WriteToFile(*s.rawConfig, clientcmd.RecommendedHomeFile); err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
			} else {
				http.Redirect(w, r, "/", http.StatusFound)
			}
		} else {
			fmt.Fprintln(os.Stderr, "default kubeconfig already exists! nothing was saved")
			http.Error(w, "", http.StatusBadRequest)
		}
	} else {
		http.Error(w, "only POST is supported", http.StatusMethodNotAllowed)
	}
}

func (server *server) loadBytesConfig(data []byte) {
	server.configLoader = &bytesConfigLoader{data}
}

func (server *server) checkKubeconfig() ServerConfigError {
	if server.pkgClient == nil {
		return server.initKubeConfig()
	} else {
		return nil
	}
}

// ensureBootstrapped checks for a valid kubeconfig (see checkKubeconfig), and whether glasskube is bootstrapped in
// the given cluster. If either of these checks fail, a ServerConfigError is returned, otherwise the result of the
// check is cached in isBootstrapped and the check will not run anymore after that. After the first successful check,
// additional components are intialized (which can only be done once glasskube is known to be bootstrapped) –
// see initWhenBootstrapped
func (server *server) ensureBootstrapped(ctx context.Context) ServerConfigError {
	if server.isBootstrapped {
		return nil
	}
	if err := server.checkKubeconfig(); err != nil {
		return err
	}

	isBootstrapped, err := bootstrap.IsBootstrapped(ctx, server.restConfig)
	if !isBootstrapped || err != nil {
		if err != nil {
			fmt.Fprintf(os.Stderr, "\nFailed to check whether Glasskube is bootstrapped: %v\n\n", err)
		}
		return newBootstrapErr(err)
	}
	server.isBootstrapped = isBootstrapped
	server.initWhenBootstrapped(ctx)
	return nil
}

func (server *server) initKubeConfig() ServerConfigError {
	restConfig, rawConfig, err := server.LoadConfig()
	if err != nil {
		return newKubeconfigErr(err)
	}
	client, err := client.New(restConfig)
	if err != nil {
		return newKubeconfigErr(err)
	}
	telemetry.InitClient(restConfig)

	server.restConfig = restConfig
	server.rawConfig = rawConfig
	server.nonCachedClient = client // this should never be overridden
	server.pkgClient = client       // be aware that server.pkgClient is overridden with the cached client once bootstrap check succeeded
	return nil
}

func (server *server) initWhenBootstrapped(ctx context.Context) {
	server.k8sClient = kubernetes.NewForConfigOrDie(server.restConfig)
	server.initCachedClient(context.WithoutCancel(ctx))
	server.initClientDependentComponents()
	factory := informers.NewSharedInformerFactory(server.k8sClient, 0)
	c := make(chan struct{})
	namespaceLister := factory.Core().V1().Namespaces().Lister()
	server.namespaceLister = &namespaceLister
	configMapLister := factory.Core().V1().ConfigMaps().Lister()
	server.configMapLister = &configMapLister
	secretLister := factory.Core().V1().Secrets().Lister()
	server.secretLister = &secretLister
	factory.Start(c)
}

func (server *server) initClientDependentComponents() {
	server.repoClientset = repoclient.NewClientset(
		clientadapter.NewPackageClientAdapter(server.pkgClient),
		clientadapter.NewKubernetesClientAdapter(server.k8sClient),
	)
	server.templates.repoClientset = server.repoClientset
	server.dependencyMgr = dependency.NewDependencyManager(
		clientadapter.NewPackageClientAdapter(server.pkgClient),
		server.repoClientset,
	)
	server.valueResolver = manifestvalues.NewResolver(
		clientadapter.NewPackageClientAdapter(server.pkgClient),
		clientadapter.NewKubernetesClientAdapter(server.k8sClient),
	)
}

func (server *server) initCachedClient(ctx context.Context) {
	clusterPackageStore, clusterPackageController := server.initClusterPackageStoreAndController(ctx)
	packageStore, packageController := server.initPackageStoreAndController(ctx)
	packageInfoStore, packageInfoController := server.initPackageInfoStoreAndController(ctx)
	packageRepoStore, packageRepoController := server.initPackageRepoStoreAndController(ctx)
	server.pkgClient = server.nonCachedClient.WithStores(clusterPackageStore, packageStore, packageInfoStore, packageRepoStore)

	clpkgVerifier := newVerifier(server.restConfig, clusterPackageVerifyLister)
	pkgVerifier := newVerifier(server.restConfig, packageVerifyLister)
	pkgInfoVerifier := newVerifier(server.restConfig, packageInfoVerifyLister)
	pkgRepoVerifier := newVerifier(server.restConfig, packageRepoVerifyLister)

	go clusterPackageController.Run(ctx.Done())
	go packageController.Run(ctx.Done())
	go packageInfoController.Run(ctx.Done())
	go packageRepoController.Run(ctx.Done())

	go server.broadcastUpdatesWhenInitiallySynced(clusterPackageController, packageController, packageInfoController, packageRepoController)

	go func() {
		for {
			select {
			case err := <-clpkgVerifier.errCh:
				server.handleVerificationError(err)
			case err := <-pkgVerifier.errCh:
				server.handleVerificationError(err)
			case err := <-pkgInfoVerifier.errCh:
				server.handleVerificationError(err)
			case err := <-pkgRepoVerifier.errCh:
				server.handleVerificationError(err)
			}
			server.shutdown()
		}
	}()

	go clpkgVerifier.start(ctx, server.pkgClient, 5)
	go pkgVerifier.start(ctx, server.pkgClient, 10)
	go pkgInfoVerifier.start(ctx, server.pkgClient, 10)
	go pkgRepoVerifier.start(ctx, server.pkgClient, 30)
}

func (s *server) broadcastUpdatesWhenInitiallySynced(controllers ...cache.Controller) {
	tick := time.NewTicker(500 * time.Millisecond)
	defer tick.Stop()
	for {
		if s.allControllersInitiallySynced(controllers...) {
			var allPkgs []ctrlpkg.Package

			var clpkgs v1alpha1.ClusterPackageList
			if err := s.pkgClient.ClusterPackages().GetAll(context.TODO(), &clpkgs); err != nil {
				fmt.Fprintf(os.Stderr, "failed to get all clusterpackages to broadcast all updates: %v\n", err)
			} else {
				for _, clpkg := range clpkgs.Items {
					p := &clpkg
					allPkgs = append(allPkgs, p)
				}
			}

			var pkgs v1alpha1.PackageList
			if err := s.pkgClient.Packages("").GetAll(context.TODO(), &pkgs); err != nil {
				fmt.Fprintf(os.Stderr, "failed to get all packages to broadcast all updates: %v\n", err)
			} else {
				for _, pkg := range pkgs.Items {
					p := &pkg
					allPkgs = append(allPkgs, p)
				}
			}

			s.broadcaster.UpdatesAvailable(refresh.RefreshTriggerAll, allPkgs...)
			break
		}
		<-tick.C
	}
}

func (s *server) allControllersInitiallySynced(controllers ...cache.Controller) bool {
	for _, c := range controllers {
		if !c.HasSynced() {
			return false
		}
	}
	return true
}

func (s *server) handleVerificationError(err error) {
	fmt.Fprintf(os.Stderr, "\n\n\n\nOUT OF SYNC – Local cache is probably outdated: %v\n", err)
	fmt.Fprintf(os.Stderr, "This is a known issue, see https://github.com/glasskube/glasskube/issues/838 – "+
		"As a consequence, the UI will appear stuck.\n")
	fmt.Fprintf(os.Stderr, "The server will stop now, please restart it manually and reload the UI in the browser! "+
		"(Of course we will fix this, sorry.)\n\n\n\n\n")
	telemetry.ReportCacheVerificationError(err)
}

func (s *server) enrichContext(h http.Handler) http.Handler {
	return &handler.ContextEnrichingHandler{Source: s, Handler: h}
}

func (s *server) requireReady(h http.HandlerFunc) http.Handler {
	return &handler.PreconditionHandler{
		Precondition: func(r *http.Request) error {
			err := s.ensureBootstrapped(r.Context())
			if err != nil {
				return err
			}
			return nil
		},
		Handler:       h,
		FailedHandler: handleConfigError,
	}
}

func (s *server) requireKubeconfig(h http.HandlerFunc) http.Handler {
	return &handler.PreconditionHandler{
		Precondition:  func(r *http.Request) error { return s.checkKubeconfig() },
		Handler:       h,
		FailedHandler: handleConfigError,
	}
}

func handleConfigError(w http.ResponseWriter, r *http.Request, err error) {
	if sce, ok := err.(ServerConfigError); ok {
		if sce.BootstrapMissing() {
			http.Redirect(w, r, "/bootstrap", http.StatusFound)
		} else {
			http.Redirect(w, r, "/support", http.StatusFound)
		}
	} else {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

func defaultKubeconfigExists() bool {
	if _, err := os.Stat(clientcmd.RecommendedHomeFile); err == nil {
		return true
	} else {
		if !errors.Is(err, os.ErrNotExist) {
			fmt.Fprintf(os.Stderr, "could not check kubeconfig file: %v", err)
		}
		return false
	}
}

func (s *server) initClusterPackageStoreAndController(ctx context.Context) (cache.Store, cache.Controller) {
	pkgClient := s.nonCachedClient
	return cache.NewInformerWithOptions(cache.InformerOptions{
		ListerWatcher: &cache.ListWatch{
			ListFunc: func(options metav1.ListOptions) (runtime.Object, error) {
				var pkgList v1alpha1.ClusterPackageList
				err := pkgClient.ClusterPackages().GetAll(ctx, &pkgList)
				return &pkgList, err
			},
			WatchFunc: func(options metav1.ListOptions) (watch.Interface, error) {
				return pkgClient.ClusterPackages().Watch(ctx, options)
			},
		},
		ObjectType: &v1alpha1.ClusterPackage{},
		Handler: cache.ResourceEventHandlerFuncs{
			AddFunc: func(obj any) {
				if pkg, ok := obj.(*v1alpha1.ClusterPackage); ok {
					s.broadcaster.UpdatesAvailableForPackage(nil, pkg)
				}
			},
			UpdateFunc: func(oldObj, newObj any) {
				if oldPkg, ok := oldObj.(*v1alpha1.ClusterPackage); ok {
					if newPkg, ok := newObj.(*v1alpha1.ClusterPackage); ok {
						s.broadcaster.UpdatesAvailableForPackage(oldPkg, newPkg)
					}
				}
			},
			DeleteFunc: func(obj any) {
				if pkg, ok := obj.(*v1alpha1.ClusterPackage); ok {
					s.broadcaster.UpdatesAvailableForPackage(pkg, nil)
					fwName := pkg.GetName()
					s.forwardersMutex.Lock()
					if result, ok := s.forwarders[fwName]; ok {
						result.Stop()
						delete(s.forwarders, fwName)
					}
					s.forwardersMutex.Unlock()
				}
			},
		},
	})
}

func (s *server) initPackageStoreAndController(ctx context.Context) (cache.Store, cache.Controller) {
	pkgClient := s.nonCachedClient
	return cache.NewInformerWithOptions(cache.InformerOptions{
		ListerWatcher: &cache.ListWatch{
			ListFunc: func(options metav1.ListOptions) (runtime.Object, error) {
				var pkgList v1alpha1.PackageList
				err := pkgClient.Packages("").GetAll(ctx, &pkgList)
				return &pkgList, err
			},
			WatchFunc: func(options metav1.ListOptions) (watch.Interface, error) {
				return pkgClient.Packages("").Watch(ctx, options)
			},
		},
		ObjectType: &v1alpha1.Package{},
		Handler: cache.ResourceEventHandlerFuncs{
			AddFunc: func(obj any) {
				if pkg, ok := obj.(*v1alpha1.Package); ok {
					s.broadcaster.UpdatesAvailableForPackage(nil, pkg)
				}
			},
			UpdateFunc: func(oldObj, newObj any) {
				if oldPkg, ok := oldObj.(*v1alpha1.Package); ok {
					if newPkg, ok := newObj.(*v1alpha1.Package); ok {
						s.broadcaster.UpdatesAvailableForPackage(oldPkg, newPkg)
					}
				}
			},
			DeleteFunc: func(obj any) {
				if pkg, ok := obj.(*v1alpha1.Package); ok {
					s.broadcaster.UpdatesAvailableForPackage(pkg, nil)
					fwName := cache.ObjectName{Namespace: pkg.GetNamespace(), Name: pkg.GetName()}.String()
					s.forwardersMutex.Lock()
					if result, ok := s.forwarders[fwName]; ok {
						result.Stop()
						delete(s.forwarders, fwName)
					}
					s.forwardersMutex.Unlock()
				}
			},
		},
	})
}

func (s *server) initPackageInfoStoreAndController(ctx context.Context) (cache.Store, cache.Controller) {
	pkgClient := s.nonCachedClient
	return cache.NewInformerWithOptions(cache.InformerOptions{
		ListerWatcher: &cache.ListWatch{
			ListFunc: func(options metav1.ListOptions) (runtime.Object, error) {
				var packageInfoList v1alpha1.PackageInfoList
				err := pkgClient.PackageInfos().GetAll(ctx, &packageInfoList)
				return &packageInfoList, err
			},
			WatchFunc: func(options metav1.ListOptions) (watch.Interface, error) {
				return pkgClient.PackageInfos().Watch(ctx, options)
			},
		},
		ObjectType: &v1alpha1.PackageInfo{},
		Handler:    cache.ResourceEventHandlerFuncs{},
	})
}

func (s *server) initPackageRepoStoreAndController(ctx context.Context) (cache.Store, cache.Controller) {
	pkgClient := s.nonCachedClient
	return cache.NewInformerWithOptions(cache.InformerOptions{
		ListerWatcher: &cache.ListWatch{
			ListFunc: func(options metav1.ListOptions) (runtime.Object, error) {
				var repositoryList v1alpha1.PackageRepositoryList
				err := pkgClient.PackageRepositories().GetAll(ctx, &repositoryList)
				return &repositoryList, err
			},
			WatchFunc: func(options metav1.ListOptions) (watch.Interface, error) {
				return pkgClient.PackageRepositories().Watch(ctx, options)
			},
		},
		ObjectType: &v1alpha1.PackageRepository{},
		Handler:    cache.ResourceEventHandlerFuncs{}, // TODO we might also want to update here?
	})
}

func (s *server) isUpdateAvailableForPkg(ctx context.Context, pkg ctrlpkg.Package) bool {
	if pkg.IsNil() {
		return false
	}
	return s.isUpdateAvailable(ctx, []ctrlpkg.Package{pkg})
}

func (s *server) isUpdateAvailable(ctx context.Context, pkgs []ctrlpkg.Package) bool {
	if tx, err := update.NewUpdater(ctx).Prepare(ctx, update.GetExact(pkgs)); err != nil {
		fmt.Fprintf(os.Stderr, "Error checking for updates: %v\n", err)
		return false
	} else if len(tx.ConflictItems) > 0 {
		return true
	} else {
		return !tx.IsEmpty()
	}
}
