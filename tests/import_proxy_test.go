package tests

import (
	"context"
	"fmt"
	"net/url"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"time"

	. "github.com/onsi/ginkgo"
	. "github.com/onsi/ginkgo/extensions/table"
	. "github.com/onsi/gomega"
	"github.com/onsi/gomega/types"

	configclient "github.com/openshift/client-go/config/clientset/versioned"
	corev1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"kubevirt.io/containerized-data-importer/pkg/common"
	"kubevirt.io/containerized-data-importer/pkg/controller"
	"kubevirt.io/containerized-data-importer/tests/framework"
	"kubevirt.io/containerized-data-importer/tests/utils"

	cdiv1 "kubevirt.io/containerized-data-importer-api/pkg/apis/core/v1beta1"

	ocpconfigv1 "github.com/openshift/api/config/v1"
)

const (
	username                 = "foo"
	password                 = "bar"
	proxyHTTPPort            = "8080"
	proxyHTTPPortWithAuth    = "8081"
	proxyTLSHTTPPort         = "443"
	proxyTLSHTTPPortWithAuth = "444"
	proxyServerName          = "cdi-test-proxy"
	fileHostName             = "cdi-file-host"
	tinyCoreQcow2            = "tinyCore.qcow2"
	tinyCoreIso              = "tinyCore.iso"
	tinyCoreIsoGz            = "tinyCore.iso.gz"
	tlsPort                  = ":443"
	tlsAuthPort              = ":444"
	authPort                 = ":81"
	port                     = ":80"
	trustedCaProxyConfigName = "user-ca-bundle"
	cdiProxyCaConfigMapName  = "cdi-test-proxy-certs"
)

var _ = Describe("Import Proxy tests", func() {
	var (
		dvName               string
		ocpClient            *configclient.Clientset
		clusterWideProxySpec *ocpconfigv1.ProxySpec
		config               *cdiv1.CDIConfig
		origSpec             *cdiv1.CDIConfigSpec
		err                  error
	)

	type importProxyTestArguments struct {
		name          string
		size          string
		noProxy       string
		imgName       string
		isHTTPS       bool
		withBasicAuth bool
		expected      func() types.GomegaMatcher
	}

	f := framework.NewFramework("import-proxy-func-test")

	BeforeEach(func() {
		if utils.IsOpenshift(f.K8sClient) {
			By("Initializing OpenShift client")
			var err error
			ocpClient, err = configclient.NewForConfig(f.RestConfig)
			Expect(err).ToNot(HaveOccurred())

			By("Storing the OpenShift Cluster Wide Proxy original configuration")
			clusterWideProxy, err := ocpClient.ConfigV1().Proxies().Get(context.TODO(), controller.ClusterWideProxyName, metav1.GetOptions{})
			Expect(err).ToNot(HaveOccurred())
			clusterWideProxySpec = clusterWideProxy.Spec.DeepCopy()
		}
		By("Saving original CDIConfig")
		config, err = f.CdiClient.CdiV1beta1().CDIConfigs().Get(context.TODO(), common.ConfigName, metav1.GetOptions{})
		Expect(err).ToNot(HaveOccurred())
		origSpec = config.Spec.DeepCopy()
	})

	AfterEach(func() {
		By("Deleting DataVolume")
		err := utils.DeleteDataVolume(f.CdiClient, f.Namespace.Name, dvName)
		Expect(err).ToNot(HaveOccurred())
		Eventually(func() bool {
			_, err := f.K8sClient.CoreV1().PersistentVolumeClaims(f.Namespace.Name).Get(context.TODO(), dvName, metav1.GetOptions{})
			return k8serrors.IsNotFound(err)
		}, timeout, pollingInterval).Should(BeTrue())

		if utils.IsOpenshift(f.K8sClient) {
			By("Reverting the cluster wide proxy spec to the original configuration")
			cleanClusterWideProxy(ocpClient, clusterWideProxySpec)
		}

		By("Restoring CDIConfig to original state")
		err = utils.UpdateCDIConfig(f.CrClient, func(config *cdiv1.CDIConfigSpec) {
			origSpec.DeepCopyInto(config)
		})

		Eventually(func() bool {
			config, err = f.CdiClient.CdiV1beta1().CDIConfigs().Get(context.TODO(), common.ConfigName, metav1.GetOptions{})
			Expect(err).ToNot(HaveOccurred())
			return reflect.DeepEqual(config.Spec, *origSpec)
		}, 30*time.Second, time.Second).Should(BeTrue())
	})

	Context("[Destructive]", func() {
		DescribeTable("should", func(args importProxyTestArguments) {

			now := time.Now()
			var proxyHTTPURL string
			var proxyHTTPSURL string
			By("Setting proxy to point to test proxy in namespace " + f.CdiInstallNs)
			if args.isHTTPS {
				proxyHTTPSURL = createProxyURL(args.isHTTPS, args.withBasicAuth, f.CdiInstallNs)
			} else {
				proxyHTTPURL = createProxyURL(args.isHTTPS, args.withBasicAuth, f.CdiInstallNs)
			}
			noProxy := args.noProxy
			imgURL := createImgURL(args.isHTTPS, args.withBasicAuth, args.imgName, f.CdiInstallNs)
			dvName = args.name

			By("Updating CDIConfig with ImportProxy configuration")
			updateProxy(f, proxyHTTPURL, proxyHTTPSURL, noProxy, ocpClient)

			By(fmt.Sprintf("Creating new datavolume %s", dvName))
			dv := createHTTPDataVolume(f, dvName, args.size, imgURL, args.isHTTPS, args.withBasicAuth)
			dataVolume, err := utils.CreateDataVolumeFromDefinition(f.CdiClient, f.Namespace.Name, dv)
			Expect(err).ToNot(HaveOccurred())

			By("Verifying pvc was created")
			pvc, err := utils.WaitForPVC(f.K8sClient, dataVolume.Namespace, dvName)
			Expect(err).ToNot(HaveOccurred())
			f.ForceBindIfWaitForFirstConsumer(pvc)
			By(fmt.Sprintf("Waiting for datavolume to match phase %s", string(cdiv1.Succeeded)))
			err = utils.WaitForDataVolumePhase(f, f.Namespace.Name, cdiv1.Succeeded, dv.Name)
			Expect(err).ToNot(HaveOccurred())

			By("Checking the importer pod information in the proxy log to verify if the requests were proxied")
			verifyImporterPodInfoInProxyLogs(f, dataVolume, imgURL, args.isHTTPS, now, args.expected)
		},
			Entry("succeed creating import dv with a proxied server (http)", importProxyTestArguments{
				name:          "dv-import-http-proxy",
				size:          "1Gi",
				noProxy:       "",
				imgName:       tinyCoreQcow2,
				isHTTPS:       false,
				withBasicAuth: false,
				expected:      BeTrue}),
			Entry("succeed creating iso import dv with a proxied server (http)", importProxyTestArguments{
				name:          "dv-import-http-proxy",
				size:          "1Gi",
				noProxy:       "",
				imgName:       tinyCoreIso,
				isHTTPS:       false,
				withBasicAuth: false,
				expected:      BeTrue}),
			Entry("succeed creating iso.gz import dv with a proxied server (http)", importProxyTestArguments{
				name:          "dv-import-http-proxy",
				size:          "1Gi",
				noProxy:       "",
				imgName:       tinyCoreIsoGz,
				isHTTPS:       false,
				withBasicAuth: false,
				expected:      BeTrue}),
			Entry("succeed creating import dv with a proxied server (http) with basic autentication", importProxyTestArguments{
				name:          "dv-import-http-proxy-auth",
				size:          "1Gi",
				noProxy:       "",
				imgName:       tinyCoreQcow2,
				isHTTPS:       false,
				withBasicAuth: true,
				expected:      BeTrue}),
			Entry("succeed creating iso import dv with a proxied server (http) with basic autentication", importProxyTestArguments{
				name:          "dv-import-http-proxy-auth",
				size:          "1Gi",
				noProxy:       "",
				imgName:       tinyCoreIso,
				isHTTPS:       false,
				withBasicAuth: true,
				expected:      BeTrue}),
			Entry("succeed creating iso.gz import dv with a proxied server (http) with basic autentication", importProxyTestArguments{
				name:          "dv-import-http-proxy-auth",
				size:          "1Gi",
				noProxy:       "",
				imgName:       tinyCoreIsoGz,
				isHTTPS:       false,
				withBasicAuth: true,
				expected:      BeTrue}),
			Entry("succeed creating import dv with a proxied server (https) with the target server with tls", importProxyTestArguments{
				name:          "dv-import-https-proxy",
				size:          "1Gi",
				noProxy:       "",
				imgName:       tinyCoreQcow2,
				isHTTPS:       true,
				withBasicAuth: false,
				expected:      BeTrue}),
			Entry("succeed creating iso import dv with a proxied server (https) with the target server with tls", importProxyTestArguments{
				name:          "dv-import-https-proxy",
				size:          "1Gi",
				noProxy:       "",
				imgName:       tinyCoreIso,
				isHTTPS:       true,
				withBasicAuth: false,
				expected:      BeTrue}),
			Entry("succeed creating iso.gz import dv with a proxied server (https) with the target server with tls", importProxyTestArguments{
				name:          "dv-import-https-proxy",
				size:          "1Gi",
				noProxy:       "",
				imgName:       tinyCoreIsoGz,
				isHTTPS:       true,
				withBasicAuth: false,
				expected:      BeTrue}),
			Entry("succeed creating import dv with a proxied server (https) with basic autentication and the target server with tls", importProxyTestArguments{
				name:          "dv-import-https-proxy-auth",
				size:          "1Gi",
				noProxy:       "",
				imgName:       tinyCoreQcow2,
				isHTTPS:       true,
				withBasicAuth: true,
				expected:      BeTrue}),
			Entry("succeed creating iso import dv with a proxied server (https) with basic autentication and the target server with tls", importProxyTestArguments{
				name:          "dv-import-https-proxy-auth",
				size:          "1Gi",
				noProxy:       "",
				imgName:       tinyCoreIso,
				isHTTPS:       true,
				withBasicAuth: true,
				expected:      BeTrue}),
			Entry("succeed creating iso import dv with a proxied server (https) with basic autentication and the target server with tls", importProxyTestArguments{
				name:          "dv-import-https-proxy-auth",
				size:          "1Gi",
				noProxy:       "",
				imgName:       tinyCoreIsoGz,
				isHTTPS:       true,
				withBasicAuth: true,
				expected:      BeTrue}),
			Entry("succeed creating import dv with a proxied server (http) but bypassing the proxy", importProxyTestArguments{
				name:          "dv-import-noproxy",
				size:          "1Gi",
				noProxy:       "*",
				imgName:       tinyCoreQcow2,
				isHTTPS:       false,
				withBasicAuth: false,
				expected:      BeFalse}),
			Entry("succeed creating import dv with a proxied server (http) but bypassing the proxy", importProxyTestArguments{
				name:          "dv-import-noproxy",
				size:          "1Gi",
				noProxy:       "*",
				imgName:       tinyCoreIso,
				isHTTPS:       false,
				withBasicAuth: false,
				expected:      BeFalse}),
		)

		DescribeTable("should proxy registry imports", func(isHTTPS, hasAuth bool) {
			now := time.Now()
			By("Updating CDIConfig with ImportProxy configuration")
			updateProxy(f, "", createProxyURL(isHTTPS, hasAuth, f.CdiInstallNs), "", ocpClient)

			By("Creating new datavolume")
			dv := utils.NewDataVolumeWithRegistryImport("import-dv", "1Gi", fmt.Sprintf(utils.TinyCoreIsoRegistryURL, f.CdiInstallNs))
			dv.Annotations[controller.AnnPodRetainAfterCompletion] = "true"
			cm, err := utils.CopyRegistryCertConfigMap(f.K8sClient, f.Namespace.Name, f.CdiInstallNs)
			Expect(err).To(BeNil())
			dv.Spec.Source.Registry.CertConfigMap = &cm
			dv, err = utils.CreateDataVolumeFromDefinition(f.CdiClient, f.Namespace.Name, dv)
			Expect(err).To(BeNil())
			dvName = dv.Name

			pvc, err := utils.WaitForPVC(f.K8sClient, dv.Namespace, dv.Name)
			Expect(err).ToNot(HaveOccurred())
			f.ForceBindIfWaitForFirstConsumer(pvc)
			By(fmt.Sprintf("Waiting for datavolume to match phase %s", string(cdiv1.Succeeded)))
			err = utils.WaitForDataVolumePhase(f, f.Namespace.Name, cdiv1.Succeeded, dv.Name)
			Expect(err).ToNot(HaveOccurred())

			By("Checking the importer pod information in the proxy log to verify if the requests were proxied")
			verifyImporterPodInfoInProxyLogs(f, dv, tinyCoreQcow2, true, now, BeTrue)
		},
			Entry("with http proxy, no auth", false, false),
			Entry("with http proxy, auth", false, true),
			Entry("with https proxy, no auth", true, false),
			Entry("with https proxy, auth", true, true),
		)
	})
})

func updateProxy(f *framework.Framework, proxyHTTPURL, proxyHTTPSURL, noProxy string, ocpClient *configclient.Clientset) {
	By("Updating CDIConfig with ImportProxy configuration")
	if !utils.IsOpenshift(f.K8sClient) {
		proxyCAConfigMapName, err := utils.CopyConfigMap(f.K8sClient, f.CdiInstallNs, cdiProxyCaConfigMapName, f.Namespace.Name, trustedCaProxyConfigName, "")
		Expect(err).ToNot(HaveOccurred())
		updateCDIConfigProxy(f, proxyHTTPURL, proxyHTTPSURL, noProxy, proxyCAConfigMapName)
	} else {
		_, err := utils.CopyConfigMap(f.K8sClient, f.CdiInstallNs, cdiProxyCaConfigMapName, f.Namespace.Name, "proxy-test-ca", "")
		Expect(err).ToNot(HaveOccurred())
		clusterWideProxyCAConfigMapName, err := utils.CopyConfigMap(f.K8sClient, f.CdiInstallNs, cdiProxyCaConfigMapName, "openshift-config", "proxy-test-ca", "ca-bundle.crt")
		Expect(err).ToNot(HaveOccurred())
		updateCDIConfigByUpdatingTheClusterWideProxy(f, ocpClient, proxyHTTPURL, proxyHTTPSURL, noProxy, clusterWideProxyCAConfigMapName)
	}
}

func createProxyURL(isHTTPS, withBasicAuth bool, namespace string) string {
	var auth string
	protocol := "http"
	port := proxyHTTPPort
	if isHTTPS {
		protocol = "https"
		port = proxyTLSHTTPPort
	}
	if withBasicAuth {
		port = proxyHTTPPortWithAuth
		auth = fmt.Sprintf("%s:%s@", username, password)
		if isHTTPS {
			port = proxyTLSHTTPPortWithAuth
		}
	}
	return fmt.Sprintf("%s://%s%s.%s:%s", protocol, auth, proxyServerName, namespace, port)
}

func createImgURL(withHTTPS, withAuth bool, imgName, namespace string) string {
	protocol := "http"
	imgPort := port
	if withAuth {
		imgPort = authPort
	}
	if withHTTPS {
		protocol = "https"
		imgPort = tlsPort
		if withAuth {
			imgPort = tlsAuthPort
		}
	}
	return fmt.Sprintf("%s://%s.%s%s/%s", protocol, fileHostName, namespace, imgPort, imgName)
}

func createHTTPDataVolume(f *framework.Framework, dataVolumeName, size, url string, isHTTPS, withBasicAuth bool) *cdiv1.DataVolume {
	dataVolume := utils.NewDataVolumeWithHTTPImport(dataVolumeName, size, url)
	dataVolume.Annotations[controller.AnnPodRetainAfterCompletion] = "true"
	if isHTTPS {
		cm, err := utils.CopyFileHostCertConfigMap(f.K8sClient, f.Namespace.Name, f.CdiInstallNs)
		Expect(err).To(BeNil())
		dataVolume.Spec.Source.HTTP.CertConfigMap = cm
	}
	if withBasicAuth {
		stringData := map[string]string{
			common.KeyAccess: utils.AccessKeyValue,
			common.KeySecret: utils.SecretKeyValue,
		}
		s, _ := utils.CreateSecretFromDefinition(f.K8sClient, utils.NewSecretDefinition(nil, stringData, nil, f.Namespace.Name, "mysecret"))
		dataVolume.Spec.Source.HTTP.SecretRef = s.Name
	}
	return dataVolume
}

func updateCDIConfigProxy(f *framework.Framework, proxyHTTPURL, proxyHTTPSURL, noProxy, trustedCa string) {
	err := utils.UpdateCDIConfig(f.CrClient, func(config *cdiv1.CDIConfigSpec) {
		config.ImportProxy = &cdiv1.ImportProxy{
			HTTPProxy:      &proxyHTTPURL,
			HTTPSProxy:     &proxyHTTPSURL,
			NoProxy:        &noProxy,
			TrustedCAProxy: &trustedCa,
		}
	})
	Expect(err).ToNot(HaveOccurred())
}

// updateCDIConfigByUpdatingTheClusterWideProxy changes the OpenShift cluster-wide proxy configuration, but we do not want in this test to have the OpenShift API behind the proxy since it might break OpenShift because of proxy hijacking.
// Then, for testing the importer pod using the proxy configuration from the cluster-wide proxy, we disable the proxy in the cluster-wide proxy obj with noProxy="*", and enable the proxy in the CDIConfig to test the importer pod with proxy configurations.
func updateCDIConfigByUpdatingTheClusterWideProxy(f *framework.Framework, ocpClient *configclient.Clientset, proxyHTTPURL, proxyHTTPSURL, noProxy, trustedCa string) {
	By("Updating OpenShift Cluster Wide Proxy with ImportProxy urls")
	updateClusterWideProxyObj(ocpClient, proxyHTTPURL, proxyHTTPSURL, noProxy, trustedCa)

	By("Waiting OpenShift Cluster Wide Proxy reconcile")
	// the default OpenShift no_proxy configuration only appears in the proxy object after and http(s) url is updated
	Eventually(func() bool {
		cwproxy, err := ocpClient.ConfigV1().Proxies().Get(context.TODO(), controller.ClusterWideProxyName, metav1.GetOptions{})
		Expect(err).ToNot(HaveOccurred())
		fmt.Fprintf(GinkgoWriter, "INFO: status HTTP: %s, proxyHTTPURL: %s, status HTTPS: %s, proxyHTTPSURL: %s,\n", cwproxy.Status.HTTPProxy, proxyHTTPURL, cwproxy.Status.HTTPSProxy, proxyHTTPSURL)
		if cwproxy.Status.HTTPProxy == proxyHTTPURL && cwproxy.Status.HTTPSProxy == proxyHTTPSURL {
			return true
		}
		return false
	}, time.Second*60, time.Second).Should(BeTrue())

	By("Waiting CDIConfig reconcile")
	Eventually(func() bool {
		config, err := f.CdiClient.CdiV1beta1().CDIConfigs().Get(context.TODO(), common.ConfigName, metav1.GetOptions{})
		Expect(err).ToNot(HaveOccurred())
		cdiHTTP, _ := controller.GetImportProxyConfig(config, common.ImportProxyHTTP)
		cdiHTTPS, _ := controller.GetImportProxyConfig(config, common.ImportProxyHTTPS)
		cdiNoProxy, _ := controller.GetImportProxyConfig(config, common.ImportProxyNoProxy)
		fmt.Fprintf(GinkgoWriter, "INFO: cdiHTTP: %s, proxyHTTPURL: %s, cdiHTTPS: %s, proxyHTTPSURL: %s, cdiNoProxy: %s\n", cdiHTTP, proxyHTTPURL, cdiHTTPS, proxyHTTPSURL, cdiNoProxy)
		return cdiHTTP == proxyHTTPURL && cdiHTTPS == proxyHTTPSURL
	}, time.Second*120, time.Second).Should(BeTrue())
}

func updateClusterWideProxyObj(ocpClient *configclient.Clientset, HTTPProxy, HTTPSProxy, NoProxy, trustedCa string) {
	proxy, err := ocpClient.ConfigV1().Proxies().Get(context.TODO(), controller.ClusterWideProxyName, metav1.GetOptions{})
	if k8serrors.IsNotFound(err) {
		Skip("This OpenShift cluster version does not have a Cluster Wide Proxy object")
	}
	Expect(err).ToNot(HaveOccurred())
	proxy.Spec.HTTPProxy = HTTPProxy
	proxy.Spec.HTTPSProxy = HTTPSProxy
	proxy.Spec.NoProxy = NoProxy
	proxy.Spec.TrustedCA.Name = trustedCa
	_, err = ocpClient.ConfigV1().Proxies().Update(context.TODO(), proxy, metav1.UpdateOptions{})
	Expect(err).ToNot(HaveOccurred())
}

// verifyImporterPodInfoInProxyLogs verifiy if the importer pod request (method, url and impoter pod IP) appears in the proxy log
func verifyImporterPodInfoInProxyLogs(f *framework.Framework, dataVolume *cdiv1.DataVolume, imgURL string, isHTTPS bool, since time.Time, expected func() types.GomegaMatcher) {
	podIP := getImporterPodIP(f)
	Eventually(func() bool {
		return wasPodProxied(imgURL, podIP, getProxyLog(f, since), isHTTPS)
	}, time.Second*60, time.Second).Should(expected())
}

func getImporterPodIP(f *framework.Framework) string {
	podIP := ""
	Eventually(func() string {
		pod, err := utils.FindPodByPrefix(f.K8sClient, f.Namespace.Name, common.ImporterPodName, common.CDILabelSelector)
		if err != nil || pod.Status.Phase == corev1.PodPending {
			return ""
		}
		fmt.Fprintf(GinkgoWriter, "INFO: Checking POD %s IP: %s\n", pod.Name, pod.Status.PodIP)
		podIP = pod.Status.PodIP
		return podIP
	}, time.Second*60, time.Second).Should(Not(BeEmpty()))
	return podIP
}

func getProxyLog(f *framework.Framework, since time.Time) string {
	proxyPod, err := utils.FindPodByPrefix(f.K8sClient, f.CdiInstallNs, proxyServerName, fmt.Sprintf("name=%s", proxyServerName))
	Expect(err).ToNot(HaveOccurred())
	fmt.Fprintf(GinkgoWriter, "INFO: Analyzing the proxy pod %s logs\n", proxyPod.Name)
	log, err := RunKubectlCommand(f, "logs", proxyPod.Name, "-n", proxyPod.Namespace, fmt.Sprintf("--since-time=%s", since.Format(time.RFC3339)))
	Expect(err).To(BeNil())
	return log
}

func wasPodProxied(imgURL, podIP, proxyLog string, isHTTPS bool) bool {
	lineMatcher := regexp.MustCompile("URL:(.*) SRC IP:(.*) BYTES:(\\d+)")
	httpMatcher := regexp.MustCompile("Content-Range\\[bytes (\\d+)\\-(\\d+)\\/(\\d+)\\]")
	u, _ := url.Parse(imgURL)
	res := false
	for _, line := range strings.Split(strings.TrimSuffix(proxyLog, "\n"), "\n") {
		matched := lineMatcher.FindStringSubmatch(line)
		matchedHost := ""
		matchedSrc := ""
		matchedBytes := 0
		if len(matched) > 1 {
			matchedHost = matched[1]
			matchedSrc = matched[2]
			matchedBytes, _ = strconv.Atoi(matched[3])
			if strings.Contains(matchedHost, u.Host) && strings.Contains(matchedSrc, podIP) && matchedBytes > 1024 {
				fmt.Fprintf(GinkgoWriter, "INFO: Matched: %s, %s, %s, %s, [%s]\n", matchedHost, u.Host, matchedSrc, podIP, line)
				res = true
			}
		} else {
			matched = httpMatcher.FindStringSubmatch(line)
			if len(matched) > 1 {
				matchedBytes, _ = strconv.Atoi(matched[1])
			}
			if matchedBytes > 1024 {
				fmt.Fprintf(GinkgoWriter, "INFO: Matched line: [%s]\n", line)
				res = true
			}
		}
	}
	if res {
		fmt.Fprintf(GinkgoWriter, "INFO: The import POD requests were proxied\n")
	} else {
		fmt.Fprintf(GinkgoWriter, "INFO: The import POD requests were not proxied\n")
	}
	return res
}

func cleanClusterWideProxy(ocpClient *configclient.Clientset, clusterWideProxySpec *ocpconfigv1.ProxySpec) {
	updateClusterWideProxyObj(ocpClient, clusterWideProxySpec.HTTPProxy, clusterWideProxySpec.HTTPSProxy, clusterWideProxySpec.NoProxy, clusterWideProxySpec.TrustedCA.Name)
	By("Waiting OpenShift Cluster Wide Proxy to be reset to original configuration")
	Eventually(func() bool {
		proxy, err := ocpClient.ConfigV1().Proxies().Get(context.TODO(), controller.ClusterWideProxyName, metav1.GetOptions{})
		Expect(err).ToNot(HaveOccurred())
		if proxy.Status.HTTPProxy == clusterWideProxySpec.HTTPProxy &&
			proxy.Status.HTTPSProxy == clusterWideProxySpec.HTTPSProxy {
			return true
		}
		return false
	}, timeout, pollingInterval).Should(BeTrue())
}
