package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/gen2brain/beeep"

	"github.com/ansd/lastpass-go"
	"github.com/fatih/color"
	"github.com/pkg/errors"
	"gopkg.in/yaml.v2"
)

type OperatingSystem string

var (
	Windows = OperatingSystem("windows")
	Linux   = OperatingSystem("linux")
	Mac     = OperatingSystem("macOs")
	Darwin  = OperatingSystem("darwin")
)

func (o *OperatingSystem) String() string {
	if *o == Windows {
		return "windows"
	}
	if *o == Linux || *o == Mac || *o == Darwin {
		return "linux"
	}

	return "Undefined"
}

type Targets struct {
	Username  string `yaml:"username"`
	Password  string `yaml:"password"`
	KeyName   string `yaml:"keyName"`
	Resources *struct {
		Npm *struct {
			Sources      []string `yaml:"sources,omitempty"`
			Projects     []string `yaml:"projects,omitempty"`
			RegistryName string   `yaml:"registryName"`
		} `yaml:"npm"`
		NuGet *struct {
			Sources  []string `yaml:"sources,omitempty"`
			Projects []string `yaml:"projects,omitempty"`
		} `yaml:"nuget"`
	} `yaml:"resources"`
}

func (t *Targets) Validate() error {
	if t.Resources == nil {
		return errors.New("resources can not be empty")
	}

	if len(t.Resources.Npm.Sources) == 0 {
		return errors.New("npm source can not be empty")
	}
	if len(t.Resources.NuGet.Sources) == 0 {
		return errors.New("nuGet source can not be empty")
	}

	if t.KeyName == "" {
		return errors.New("keyName can not be empty")
	}

	return nil
}

func main() {

	ctx := context.Background()
	file, err := os.Open("./targets.yaml")
	if os.IsNotExist(err) {
		log.Fatalf("targets.yaml file does not exist, please provide a file")
	}
	defer file.Close()

	var targets Targets
	buf, err := ioutil.ReadAll(file)
	if err != nil {
		log.Fatalf("targets.yaml could not read")
	}
	err = yaml.Unmarshal(buf, &targets)
	if err != nil {
		log.Fatalf("can not parse targets.yaml file: %v", err.Error())
	}

	err = targets.Validate()
	if err != nil {
		log.Fatalf(color.RedString("targets.yaml file validation was failed: %v", err))
	}

	var (
		username    = flag.String("username", targets.Username, "")
		password    = flag.String("password", targets.Password, "")
		updateLocal = flag.Bool("update-local", true, "('false' if not present)")
		jsonOnly    = flag.Bool("json", false, "Returns only json response. ('false' if not present)")
		stdout      = os.Stdout
	)
	flag.Parse()

	if *username == "" || *password == "" {
		_, _ = fmt.Fprintf(stdout, "Please provide username and password to login LastPass\n")
		return
	}

	client, _ := lastpass.NewClient(context.Background(), *username, *password)

	acs, err := client.Accounts(context.Background())
	if err != nil {
		log.Fatal("Can not access to LastPass. Are you sure about the credentials you provide?")
	}

	var credentials *struct {
		Username string `json:"username"`
		Token    string `json:"token"`
	}

	for _, ac := range acs {
		if ac.Name == targets.KeyName {
			_ = json.Unmarshal([]byte(ac.Notes), &credentials)
			jToken, err := json.MarshalIndent(&credentials, "", " ")
			if err != nil {
				log.Fatalf("There was a problem with decoding json response, please check Artifactory Token is shared with you in a JSON format")
			}

			if *jsonOnly {
				_, _ = fmt.Fprintf(stdout, string(jToken))
				return
			}
		}
	}

	if *updateLocal {
		log.Printf("I will let you develop by changing local .npmrc and .nuget.config files!")

		err := updateNuGet(targets, ctx, credentials)
		if err != nil {
			log.Println(color.YellowString("NuGet update was failed, because: %v", err))
		}
		err = updateNpm(targets, *username, credentials)
		if err != nil {
			log.Println(color.YellowString("Npm update was failed, because: %v", err))
		}

		err = copyToProjects(targets)
		if err != nil {
			log.Println(color.YellowString("Copying config files are failed, because: %v", err))
		}

		_ = beeep.Notify("Let Me Pass!", "Last changes are applied to local environment", "")
	}
}

func updateNuGet(targets Targets, ctx context.Context, tr *struct {
	Username string `json:"username"`
	Token    string `json:"token"`
}) error {
	for i, source := range targets.Resources.NuGet.Sources {
		cmd := exec.CommandContext(ctx,
			"nuget",
			"sources",
			"Add",
			"-Name", fmt.Sprintf("Nuget_%v", i),
			"-Source", source,
			"-username", tr.Username,
			"-password", tr.Token,
		)

		cmd.Stderr = os.Stderr
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr

		name := fmt.Sprintf("Nuget_%v", i)
		err := cmd.Run()
		if err != nil {
			log.Println(color.YellowString("nuget sources add got an error: %v", err))
			log.Println(color.GreenString("nuget resources trying to update..."))

			cmd = exec.CommandContext(ctx,
				"nuget",
				"sources",
				"Update",
				"-Name", name,
				"-Source", source,
				"-username", tr.Username,
				"-password", tr.Token,
			)
			err = cmd.Run()
			if err != nil {
				log.Println(color.YellowString("update nuget sources got an error: %v", err))
			} else {
				log.Println(color.GreenString("update succeeded!"))
			}
		}

		cmd = exec.CommandContext(ctx,
			"nuget",
			"setapikey",
			fmt.Sprintf("%v:%v", tr.Username, tr.Token),
			"-Source", name,
		)

		err = cmd.Run()
		if err != nil {
			return err
		}

		log.Printf(color.GreenString("NuGet Source: %v has been added successfully", source))
	}

	return nil
}

func updateNpm(
	targets Targets,
	username string,
	tr *struct {
		Username string `json:"username"`
		Token    string `json:"token"`
	}) error {

	npmTemplate := `{registryName}:registry=%v
//{registry}:_password=%v
//{registry}:username=%v
//{registry}:email=%v
//{registry}:always-auth=true
`
	npm := ""
	for _, source := range targets.Resources.Npm.Sources {
		registry := cleanProtocol(source)
		data := []byte(tr.Token)
		base64Token := base64.StdEncoding.EncodeToString(data)
		npm += strings.ReplaceAll(
			strings.ReplaceAll(
				fmt.Sprintf(npmTemplate, source, base64Token, tr.Username, username),
				"{registry}",
				registry), "{registryName}",
			targets.Resources.Npm.RegistryName)

		log.Printf(color.GreenString("Npm Source: %v has been added successfully", source))
	}

	npmRcFilePath := path.Join(GetHomeDir(), ".npmrc")
	file, err := os.OpenFile(npmRcFilePath, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0660)
	if err != nil {
		return err
	}
	defer file.Close()
	_, err = file.WriteString(npm)
	err = file.Sync()
	if err != nil {
		return err
	}

	log.Println(color.GreenString("NpmRc updated successfully"))

	return nil
}

func copyToProjects(targets Targets) error {
	for _, project := range targets.Resources.Npm.Projects {
		err := CopyFile(path.Join(GetHomeDir(), ".npmrc"), path.Join(project, ".npmrc"))
		if err != nil {
			return err
		}
	}

	for _, project := range targets.Resources.NuGet.Projects {
		err := CopyFile(GetNugetConfig(), path.Join(project, "NuGet.Config"))
		if err != nil {
			return err
		}
	}

	return nil
}

func cleanProtocol(source string) string {
	registry := strings.Replace(source, "https://", "", 1)
	registry = strings.Replace(registry, "http://", "", 1)
	return registry
}

func GetHomeDir() string {
	if runtime.GOOS == "windows" {
		return os.Getenv("APPDATA")
	}

	return os.Getenv("HOME")
}

func GetNugetConfig() string {
	home := GetHomeDir()
	isWindows := runtime.GOOS == "windows"

	if isWindows {
		return path.Join(home, "/nuget/NuGet.Config")
	}

	return path.Join(home, "/.config/NuGet/NuGet.Config")
}

func CopyFile(src, dst string) error {

	dir := filepath.Dir(dst)
	err := os.MkdirAll(dir, os.ModePerm)
	if err != nil {
		return err
	}

	sourceFile, err := os.Open(src)
	if err != nil {
		return err
	}
	defer sourceFile.Close()

	newFile, err := os.OpenFile(dst, os.O_RDWR|os.O_CREATE, os.ModePerm)
	if err != nil {
		return err
	}
	defer newFile.Close()

	_, err = io.Copy(newFile, sourceFile)
	if err != nil {
		return err
	}

	return nil
}
