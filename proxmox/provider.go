package proxmox

import (
	"crypto/tls"
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"

	pxapi "github.com/Telmate/proxmox-api-go/proxmox"
	"github.com/hashicorp/terraform-plugin-sdk/v2/helper/schema"
)

type providerConfiguration struct {
	Client                             *pxapi.Client
	MaxParallel                        int
	CurrentParallel                    int
	MaxVMID                            int
	Mutex                              *sync.Mutex
	Cond                               *sync.Cond
	LogFile                            string
	LogLevels                          map[string]string
	DangerouslyIgnoreUnknownAttributes bool
}

// Provider - Terrafrom properties for proxmox
func Provider() *schema.Provider {
	pmOTPprompt := schema.Schema{
		Type:        schema.TypeString,
		Optional:    true,
		DefaultFunc: schema.EnvDefaultFunc("PM_OTP", ""),
		Description: "OTP 2FA code (if required)",
	}
	if os.Getenv("PM_OTP_PROMPT") == "1" {
		pmOTPprompt = schema.Schema{
			Type:        schema.TypeString,
			Required:    true,
			DefaultFunc: schema.EnvDefaultFunc("PM_OTP", nil),
			Description: "OTP 2FA code (if required)",
		}
	}
	return &schema.Provider{

		Schema: map[string]*schema.Schema{
			"pm_user": {
				Type:        schema.TypeString,
				Optional:    true,
				DefaultFunc: schema.EnvDefaultFunc("PM_USER", nil),
				Description: "Username e.g. myuser or myuser@pam",
			},
			"pm_password": {
				Type:        schema.TypeString,
				Optional:    true,
				DefaultFunc: schema.EnvDefaultFunc("PM_PASS", nil),
				Description: "Password to authenticate into proxmox",
				Sensitive:   true,
			},
			"pm_api_url": {
				Type:        schema.TypeString,
				Required:    true,
				DefaultFunc: schema.EnvDefaultFunc("PM_API_URL", nil),
				Description: "https://host.fqdn:8006/api2/json",
			},
			"pm_api_token_id": {
				Type:        schema.TypeString,
				Optional:    true,
				DefaultFunc: schema.EnvDefaultFunc("PM_API_TOKEN_ID", nil),
				Description: "API TokenID e.g. root@pam!mytesttoken",
			},
			"pm_api_token_secret": {
				Type:        schema.TypeString,
				Optional:    true,
				DefaultFunc: schema.EnvDefaultFunc("PM_API_TOKEN_SECRET", nil),
				Description: "The secret uuid corresponding to a TokenID",
				Sensitive:   true,
			},
			"pm_parallel": {
				Type:     schema.TypeInt,
				Optional: true,
				Default:  4,
			},
			"pm_tls_insecure": {
				Type:        schema.TypeBool,
				Optional:    true,
				DefaultFunc: schema.EnvDefaultFunc("PM_TLS_INSECURE", false),
				Description: "By default, every TLS connection is verified to be secure. This option allows terraform to proceed and operate on servers considered insecure. For example if you're connecting to a remote host and you do not have the CA cert that issued the proxmox api url's certificate.",
			},
			"pm_log_enable": {
				Type:        schema.TypeBool,
				Optional:    true,
				Default:     false,
				Description: "Enable provider logging to get proxmox API logs",
			},
			"pm_log_levels": {
				Type:        schema.TypeMap,
				Optional:    true,
				Description: "Configure the logging level to display; trace, debug, info, warn, etc",
			},
			"pm_log_file": {
				Type:        schema.TypeString,
				Optional:    true,
				Default:     "terraform-plugin-proxmox.log",
				Description: "Write logs to this specific file",
			},
			"pm_timeout": {
				Type:     schema.TypeInt,
				Optional: true,
				Default:  300,
			},
			"pm_dangerously_ignore_unknown_attributes": {
				Type:        schema.TypeBool,
				Optional:    true,
				DefaultFunc: schema.EnvDefaultFunc("PM_DANGEROUSLY_IGNORE_UNKNOWN_ATTRIBUTES", false),
				Description: "By default this provider will exit if an unknown attribute is found. This is to prevent the accidential destruction of VMs or Data when something in the proxmox API has changed/updated and is not confirmed to work with this provider. Set this to true at your own risk. It may allow you to proceed in cases when the provider refuses to work, but be aware of the danger in doing so.",
			},
			"pm_otp": &pmOTPprompt,
		},

		ResourcesMap: map[string]*schema.Resource{
			"proxmox_vm_qemu":  resourceVmQemu(),
			"proxmox_lxc":      resourceLxc(),
			"proxmox_lxc_disk": resourceLxcDisk(),
			"proxmox_pool":     resourcePool(),
			// TODO - proxmox_storage_iso
			// TODO - proxmox_bridge
			// TODO - proxmox_vm_qemu_template
		},

		ConfigureFunc: providerConfigure,
	}
}

func providerConfigure(d *schema.ResourceData) (interface{}, error) {
	client, err := getClient(
		d.Get("pm_api_url").(string),
		d.Get("pm_user").(string),
		d.Get("pm_password").(string),
		d.Get("pm_api_token_id").(string),
		d.Get("pm_api_token_secret").(string),
		d.Get("pm_otp").(string),
		d.Get("pm_tls_insecure").(bool),
		d.Get("pm_timeout").(int),
	)
	if err != nil {
		return nil, err
	}

	// look to see what logging we should be outputting according to the provider configuration
	logLevels := make(map[string]string)
	for logger, level := range d.Get("pm_log_levels").(map[string]interface{}) {
		levelAsString, ok := level.(string)
		if ok {
			logLevels[logger] = levelAsString
		} else {
			return nil, fmt.Errorf("Invalid logging level %v for %v. Be sure to use a string.", level, logger)
		}
	}

	// actually configure logging
	// note that if enable is false here, the configuration will squash all output
	ConfigureLogger(
		d.Get("pm_log_enable").(bool),
		d.Get("pm_log_file").(string),
		logLevels,
	)

	var mut sync.Mutex
	return &providerConfiguration{
		Client:                             client,
		MaxParallel:                        d.Get("pm_parallel").(int),
		CurrentParallel:                    0,
		MaxVMID:                            -1,
		Mutex:                              &mut,
		Cond:                               sync.NewCond(&mut),
		LogFile:                            d.Get("pm_log_file").(string),
		LogLevels:                          logLevels,
		DangerouslyIgnoreUnknownAttributes: d.Get("pm_dangerously_ignore_unknown_attributes").(bool),
	}, nil
}

func getClient(pm_api_url string, pm_user string, pm_password string, pm_api_token_id string, pm_api_token_secret string, pm_otp string, pm_tls_insecure bool, pm_timeout int) (*pxapi.Client, error) {
	tlsconf := &tls.Config{InsecureSkipVerify: true}
	if !pm_tls_insecure {
		tlsconf = nil
	}

	var err error

	if pm_password != "" && pm_api_token_secret != "" {
		err = fmt.Errorf("Password and API token secret both exist, choose one or the other.")
	}

	if pm_password == "" && pm_api_token_secret == "" {
		err = fmt.Errorf("Password and API token do not exist, one of these must exist.")
	}

	if strings.Contains(pm_user, "!") && pm_password != "" {
		err = fmt.Errorf("You appear to be using an API TokenID username with your password.")
	}

	if !strings.Contains(pm_api_token_id, "!") {
		err = fmt.Errorf("Your API TokenID username should contain a !, check your API credentials.")
	}

	client, _ := pxapi.NewClient(pm_api_url, nil, tlsconf, pm_timeout)

	// User+Pass authentication
	if pm_user != "" && pm_password != "" {
		err = client.Login(pm_user, pm_password, pm_otp)
	}

	// API authentication
	if pm_api_token_id != "" && pm_api_token_secret != "" {
		// Unsure how to get an err for this
		client.SetAPIToken(pm_api_token_id, pm_api_token_secret)
	}

	if err != nil {
		return nil, err
	}
	return client, nil
}

func nextVmId(pconf *providerConfiguration) (nextId int, err error) {
	pconf.Mutex.Lock()
	defer pconf.Mutex.Unlock()
	pconf.MaxVMID, err = pconf.Client.GetNextID(pconf.MaxVMID + 1)
	if err != nil {
		return 0, err
	}
	nextId = pconf.MaxVMID
	return nextId, nil
}

type pmApiLockHolder struct {
	locked bool
	pconf  *providerConfiguration
}

func (lock *pmApiLockHolder) lock() {
	if lock.locked {
		return
	}
	lock.locked = true
	pconf := lock.pconf
	pconf.Mutex.Lock()
	for pconf.CurrentParallel >= pconf.MaxParallel {
		pconf.Cond.Wait()
	}
	pconf.CurrentParallel++
	pconf.Mutex.Unlock()
}

func (lock *pmApiLockHolder) unlock() {
	if !lock.locked {
		return
	}
	lock.locked = false
	pconf := lock.pconf
	pconf.Mutex.Lock()
	pconf.CurrentParallel--
	pconf.Cond.Signal()
	pconf.Mutex.Unlock()
}

func pmParallelBegin(pconf *providerConfiguration) *pmApiLockHolder {
	lock := &pmApiLockHolder{
		pconf:  pconf,
		locked: false,
	}
	lock.lock()
	return lock
}

func resourceId(targetNode string, resType string, vmId int) string {
	return fmt.Sprintf("%s/%s/%d", targetNode, resType, vmId)
}

var rxRsId = regexp.MustCompile("([^/]+)/([^/]+)/(\\d+)")

func parseResourceId(resId string) (targetNode string, resType string, vmId int, err error) {
	if !rxRsId.MatchString(resId) {
		return "", "", -1, fmt.Errorf("Invalid resource format: %s. Must be node/type/vmId", resId)
	}
	idMatch := rxRsId.FindStringSubmatch(resId)
	targetNode = idMatch[1]
	resType = idMatch[2]
	vmId, err = strconv.Atoi(idMatch[3])
	return
}

func clusterResourceId(resType string, resId string) string {
	return fmt.Sprintf("%s/%s", resType, resId)
}

var rxClusterRsId = regexp.MustCompile("([^/]+)/([^/]+)")

func parseClusterResourceId(resId string) (resType string, id string, err error) {
	if !rxClusterRsId.MatchString(resId) {
		return "", "", fmt.Errorf("Invalid resource format: %s. Must be type/resId", resId)
	}
	idMatch := rxClusterRsId.FindStringSubmatch(resId)
	return idMatch[1], idMatch[2], nil
}
