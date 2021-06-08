// SPDX-License-Identifier: Apache-2.0

package javamaven

import (
	"bufio"
	"encoding/xml"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"os"
	"os/exec"
	"path"
	"spdx-sbom-generator/internal/helper"
	"spdx-sbom-generator/internal/models"
	"strings"

	"github.com/vifraa/gopom"
)

// Update package supplier information
func updatePackageSuppier(mod models.Module, developers []gopom.Developer) {

	if len(developers) > 0 {
		if len(developers[0].Name) > 0 && len(developers[0].Email) > 0 {
			mod.Supplier.Type = "Person"
			mod.Supplier.Name = developers[0].Name
			mod.Supplier.Email = developers[0].Email
		} else if len(developers[0].Email) == 0 && len(developers[0].Name) > 0 {
			mod.Supplier.Type = "Person"
			mod.Supplier.Name = developers[0].Name
		}

		// check for organization tag
		if len(developers[0].Organization) > 0 {
			mod.Supplier.Type = "Organization"
		}
	}
}

// Update package download location
func updatePackageDownloadLocation(mod models.Module, distManagement gopom.DistributionManagement) {
	if len(distManagement.DownloadURL) > 0 && (strings.HasPrefix(distManagement.DownloadURL, "http") ||
		strings.HasPrefix(distManagement.DownloadURL, "https")) {
		// ******** TODO Module has only PackageHomePage, it does not have PackageDownloadLocation field
		//mod.PackageDownloadLocation = distManagement.DownloadURL
	}
}

// captures os.Stdout data and writes buffers
func stdOutCapture() func() (string, error) {
	readFromPipe, writeToPipe, err := os.Pipe()
	if err != nil {
		panic(err)
	}

	done := make(chan error, 1)

	save := os.Stdout
	os.Stdout = writeToPipe

	var buffer strings.Builder

	go func() {
		_, err := io.Copy(&buffer, readFromPipe)
		readFromPipe.Close()
		done <- err
	}()

	return func() (string, error) {
		os.Stdout = save
		writeToPipe.Close()
		err := <-done
		return buffer.String(), err
	}
}

func getDependencyList() ([]string, error) {
	done := stdOutCapture()

	cmd1 := exec.Command("mvn", "-o", "dependency:list")
	cmd2 := exec.Command("grep", ":.*:.*:.*")
	cmd3 := exec.Command("cut", "-d]", "-f2-")
	cmd4 := exec.Command("sort", "-u")
	cmd2.Stdin, _ = cmd1.StdoutPipe()
	cmd3.Stdin, _ = cmd2.StdoutPipe()
	cmd4.Stdin, _ = cmd3.StdoutPipe()
	cmd4.Stdout = os.Stdout
	_ = cmd4.Start()
	_ = cmd3.Start()
	_ = cmd2.Start()
	_ = cmd1.Run()
	_ = cmd2.Wait()
	_ = cmd3.Wait()
	_ = cmd4.Wait()

	capturedOutput, err := done()
	if err != nil {
		fmt.Println(err)
		return nil, err
	}

	s := strings.Split(capturedOutput, "\n")
	return s, err
}

func convertMavenPackageToModule(project gopom.Project) models.Module {
	// package to module
	var modName string
	if len(project.Name) == 0 {
		modName = strings.Replace(project.ArtifactID, " ", "-", -1)
	} else {
		modName = project.Name
		if strings.HasPrefix(project.Name, "$") {
			name := strings.TrimLeft(strings.TrimRight(project.Name, "}"), "${")
			if strings.HasPrefix(name, "project") {
				modName = project.Parent.ArtifactID
			}
		}
		modName = strings.Replace(modName, " ", "-", -1)
	}

	modVersion := project.Version
	if strings.HasPrefix(project.Version, "$") {
		version := strings.TrimLeft(strings.TrimRight(project.Version, "}"), "${")
		modVersion = project.Properties.Entries[version]
	}

	var mod models.Module
	mod.Name = modName
	mod.Version = modVersion
	mod.Modules = map[string]*models.Module{}
	mod.CheckSum = &models.CheckSum{
		Algorithm: models.HashAlgoSHA1,
		Value:     readCheckSum(modName),
	}
	mod.Root = true
	updatePackageSuppier(mod, project.Developers)
	updatePackageDownloadLocation(mod, project.DistributionManagement)
	if len(project.URL) > 0 {
		mod.PackageHomePage = project.URL
	}

	licensePkg, err := helper.GetLicenses(".")
	if err == nil {
		mod.LicenseDeclared = helper.BuildLicenseDeclared(licensePkg.ID)
		mod.LicenseConcluded = helper.BuildLicenseConcluded(licensePkg.ID)
		mod.Copyright = helper.GetCopyright(licensePkg.ExtractedText)
		mod.CommentsLicense = licensePkg.Comments
	}

	return mod
}

func FindInDependency(slice []gopom.Dependency, val string) (int, bool) {
	for i, item := range slice {
		if item.ArtifactID == val {
			return i, true
		}
	}
	return -1, false
}

func FindInPlugins(slice []gopom.Plugin, val string) (int, bool) {
	for i, item := range slice {
		if item.ArtifactID == val {
			return i, true
		}
	}
	return -1, false
}

func createModule(name string, version string, project gopom.Project) models.Module {
	var mod models.Module

	modVersion := version
	if strings.HasPrefix(version, "$") {
		version := strings.TrimLeft(strings.TrimRight(version, "}"), "${")
		modVersion = project.Properties.Entries[version]
	}

	name = path.Base(name)
	mod.Name = strings.Replace(name, " ", "-", -1)
	mod.Version = modVersion
	mod.Modules = map[string]*models.Module{}
	mod.CheckSum = &models.CheckSum{
		Algorithm: models.HashAlgoSHA1,
		Value:     readCheckSum(name),
	}
	return mod
}

// If parent pom.xml has modules information in it, go to individual modules pom.xml
func convertPkgModulesToModule(fpath string, moduleName string, parentPom gopom.Project) ([]models.Module, error) {
	filePath := fpath + "/" + moduleName + "/pom.xml"
	pomFile, err := os.Open(filePath)
	if err != nil {
		fmt.Println(err)
		return []models.Module{}, err
	}
	defer pomFile.Close()

	var modules []models.Module
	// read our opened xmlFile as a byte array.
	pomStr, _ := ioutil.ReadAll(pomFile)

	// Load project from string
	var project gopom.Project
	if err := xml.Unmarshal([]byte(pomStr), &project); err != nil {
		fmt.Printf("unable to unmarshal Module %s pom file. Reason: %v", moduleName, err)
		return []models.Module{}, err
	}

	var version string
	if len(project.Version) == 0 {
		// set package version as module version
		version = parentPom.Version
	} else {
		version = project.Version
	}
	parentMod := createModule(project.Name, version, project)
	modules = append(modules, parentMod)

	// Include dependecy from module pom.xml if it is not existing in ParentPom
	for _, element := range project.Dependencies {
		name := strings.Replace(element.ArtifactID, " ", "-", -1)
		_, found := FindInDependency(parentPom.Dependencies, name)
		if !found {
			_, found1 := FindInDependency(parentPom.DependencyManagement.Dependencies, name)
			if !found1 {
				mod := createModule(name, element.Version, project)
				modules = append(modules, mod)
				parentMod.Modules[mod.Name] = &mod
			}
		}
	}

	// Include plugins from module pom.xml if it is not existing in ParentPom
	for _, element := range project.Build.Plugins {
		name := strings.Replace(element.ArtifactID, " ", "-", -1)
		_, found := FindInPlugins(parentPom.Build.Plugins, name)
		if !found {
			_, found1 := FindInPlugins(parentPom.Build.PluginManagement.Plugins, name)
			if !found1 {
				mod := createModule(name, element.Version, project)
				modules = append(modules, mod)
				parentMod.Modules[mod.Name] = &mod
			}
		}
	}
	return modules, nil
}

func convertPOMReaderToModules(fpath string, lookForDepenent bool) ([]models.Module, error) {
	modules := make([]models.Module, 0)

	filePath := fpath + "/pom.xml"
	pomFile, err := os.Open(filePath)
	if err != nil {
		fmt.Println(err)
		return modules, err
	}
	defer pomFile.Close()

	// read our opened xmlFile as a byte array.
	pomStr, _ := ioutil.ReadAll(pomFile)

	// Load project from string
	var project gopom.Project
	if err := xml.Unmarshal([]byte(pomStr), &project); err != nil {
		fmt.Printf("unable to unmarshal pom file. Reason: %v", err)
		return modules, err
	}

	parentMod := convertMavenPackageToModule(project)
	modules = append(modules, parentMod)

	// iterate over dependencyManagement
	for _, dependencyManagement := range project.DependencyManagement.Dependencies {
		mod := createModule(dependencyManagement.ArtifactID, dependencyManagement.Version, project)
		modules = append(modules, mod)
		parentMod.Modules[mod.Name] = &mod
	}

	// iterate over dependencies
	for _, dep := range project.Dependencies {
		mod := createModule(dep.ArtifactID, dep.Version, project)
		modules = append(modules, mod)
		parentMod.Modules[mod.Name] = &mod
	}

	// iterate over Plugins
	for _, plugin := range project.Build.Plugins {
		mod := createModule(plugin.ArtifactID, plugin.Version, project)
		modules = append(modules, mod)
		parentMod.Modules[mod.Name] = &mod
	}

	// iterate over PluginManagement
	for _, plugin := range project.Build.PluginManagement.Plugins {
		mod := createModule(plugin.ArtifactID, plugin.Version, project)
		modules = append(modules, mod)
		parentMod.Modules[mod.Name] = &mod
	}

	dependencyList, err := getDependencyList()
	if err != nil {
		fmt.Println("error in getting mvn dependency list and parsing it")
		return modules, err
	}

	// Add additional dependency from mvn dependency list to pom.xml dependency list
	var i int
	for i < len(dependencyList)-2 { // skip 1 empty line and Finished statement line
		dependencyItem := strings.Split(dependencyList[i], ":")[1]

		found := false
		// iterate over dependencies
		for _, dep := range project.Dependencies {
			if dep.ArtifactID == dependencyItem {
				found = true
				break
			}
		}

		if !found {
			for _, dependencyManagement := range project.DependencyManagement.Dependencies {
				if dependencyManagement.ArtifactID == dependencyItem {
					found = true
					break
				}
			}
		}

		if !found {
			version := strings.Split(dependencyList[i], ":")[3]
			mod := createModule(dependencyItem, version, project)
			modules = append(modules, mod)
			parentMod.Modules[mod.Name] = &mod
		}
		i++
	}

	if lookForDepenent {
		// iterate over Modules
		for _, module := range project.Modules {
			additionalModules, err := convertPkgModulesToModule(fpath, module, project)
			if err != nil {
				// continue reading other module pom.xml file
				continue
			}
			modules = append(modules, additionalModules...)
		}
	}
	return modules, nil
}

func getTransitiveDependencyList() (map[string][]string, error) {
	path := "/tmp/JavaMavenTDTreeOutput.txt"
	os.Remove(path)

	command := exec.Command("mvn", "dependency:tree", "-DoutputType=dot", "-DappendOutput=true", "-DoutputFile=/tmp/JavaMavenTDTreeOutput.txt")
	_, err := command.Output()
	if err != nil {
		return nil, err
	}

	tdList, err := readAndgetTransitiveDependencyList()
	if err != nil {
		return nil, err
	}
	return tdList, nil
}

func readAndgetTransitiveDependencyList() (map[string][]string, error) {

	file, err := os.Open("/tmp/JavaMavenTDTreeOutput.txt")

	if err != nil {
		log.Println(err)
		return nil, err
	}

	scanner := bufio.NewScanner(file)

	scanner.Split(bufio.ScanLines)
	var text []string

	for scanner.Scan() {
		text = append(text, scanner.Text())
	}
	file.Close()

	tdList := map[string][]string{}
	handlePkgs(text, tdList)
	return tdList, nil
}

func doesDependencyExists(tdList map[string][]string, lData string, val string) bool {
	for _, item := range tdList[lData] {
		if item == val {
			return true
		}
	}
	return false
}

func handlePkgs(text []string, tdList map[string][]string) {
	i := 0
	var pkgName string
	isEmptyMainPkg := false

	for i < len(text) {
		if strings.Contains(text[i], "{") {
			pkgName = strings.Split(text[i], ":")[1]
		} else if strings.Contains(text[i], "->") {
			lhsData := strings.Split(text[i], "->")[0]
			rhsData := strings.Split(text[i], "->")[1]
			lData := strings.Split(lhsData, ":")[1]
			rData := strings.Split(rhsData, ":")[1]

			// If package name is same, add right hand side dependency
			if !isEmptyMainPkg && lData == pkgName {
				tdList[pkgName] = append(tdList[pkgName], rData)
			} else if !doesDependencyExists(tdList, lData, rData) { // check whether dependency already exists
				tdList[lData] = append(tdList[lData], rData)
			}
		} else if strings.Contains(text[i], "}") {
			if i == 1 {
				isEmptyMainPkg = true
			}
		}
		i++
	}
}

func buildDependenciesGraph(modules []models.Module, tdList map[string][]string) error {
	moduleMap := map[string]models.Module{}
	moduleIndex := map[string]int{}

	for idx, module := range modules {
		moduleMap[module.Name] = module
		moduleIndex[module.Name] = idx
	}

	for i := range tdList {
		for j := range tdList[i] {

			if len(tdList[i][j]) > 0 {
				moduleName := i
				if _, ok := moduleMap[moduleName]; !ok {
					continue
				}

				depName := tdList[i][j]
				depModule, ok := moduleMap[depName]
				if !ok {
					continue
				}

				modules[moduleIndex[moduleName]].Modules[depName] = &models.Module{
					Name:             depModule.Name,
					Version:          depModule.Version,
					Path:             depModule.Path,
					LocalPath:        depModule.LocalPath,
					Supplier:         depModule.Supplier,
					PackageURL:       depModule.PackageURL,
					CheckSum:         depModule.CheckSum,
					PackageHomePage:  depModule.PackageHomePage,
					LicenseConcluded: depModule.LicenseConcluded,
					LicenseDeclared:  depModule.LicenseDeclared,
					CommentsLicense:  depModule.CommentsLicense,
					OtherLicense:     depModule.OtherLicense,
					Copyright:        depModule.Copyright,
					PackageComment:   depModule.PackageComment,
					Root:             depModule.Root,
				}
			}
		}
	}

	return nil
}