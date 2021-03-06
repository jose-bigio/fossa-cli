package builders

import (
	"encoding/xml"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/bmatcuk/doublestar"

	"github.com/fossas/fossa-cli/log"
	"github.com/fossas/fossa-cli/module"
)

// MavenArtifact implements Dependency for Maven builds
type MavenArtifact struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

// Fetcher always returns mvn for MavenArtifact
func (m MavenArtifact) Fetcher() string {
	return "mvn"
}

// Package returns the package spec for MavenArtifact
func (m MavenArtifact) Package() string {
	return m.Name
}

// Revision returns the version spec for MavenArtifact
func (m MavenArtifact) Revision() string {
	return m.Version
}

// Dependencies is not implemented for MavenArtifact
func (m MavenArtifact) Dependencies() []module.Dependency {
	return nil
}

// POMFile represents the schema of a common pom.xml file
type POMFile struct {
	XMLName     xml.Name `xml:"project"`
	ArtifactID  string   `xml:"artifactId"`
	GroupID     string   `xml:"groupId"`
	Version     string   `xml:"version"`
	Description string   `xml:"description"`
	Name        string   `xml:"name"`
	URL         string   `xml:"url"`
}

// MavenBuilder implements Builder for Apache Maven (*.pom.xml) builds
type MavenBuilder struct {
	JavaCmd     string
	JavaVersion string

	MvnCmd     string
	MvnVersion string
}

// Initialize collects metadata on Java and Maven binaries
func (builder *MavenBuilder) Initialize() error {
	log.Logger.Debug("Initializing Maven builder...")

	// Set Java context variables
	javaCmd, javaVersion, err := which("-version", os.Getenv("JAVA_BINARY"), "java")
	if err != nil {
		log.Logger.Warningf("Could not find Java binary (try setting $JAVA_BINARY): %s", err.Error())
	}
	builder.JavaCmd = javaCmd
	builder.JavaVersion = javaVersion

	// Set Maven context variables
	mavenCmd, mavenVersion, err := which("--version", os.Getenv("MAVEN_BINARY"), "mvn")
	if err != nil {
		return fmt.Errorf("could not find Maven binary (try setting $MAVEN_BINARY): %s", err.Error())
	}
	builder.MvnCmd = mavenCmd
	builder.MvnVersion = mavenVersion

	log.Logger.Debugf("Done initializing Maven builder: %#v", builder)
	return nil
}

// Build runs `mvn install -DskipTests -Drat.skip=true` and cleans with `mvn clean`
func (builder *MavenBuilder) Build(m module.Module, force bool) error {
	log.Logger.Debugf("Running Maven build: %#v %#v", m, force)

	if force {
		_, _, err := runLogged(m.Dir, builder.MvnCmd, "clean")
		if err != nil {
			return fmt.Errorf("could not remove Maven cache: %s", err.Error())
		}
	}

	_, _, err := runLogged(m.Dir, builder.MvnCmd, "install", "-DskipTests", "-Drat.skip=true")
	if err != nil {
		return fmt.Errorf("could not run Maven build: %s", err.Error())
	}

	log.Logger.Debug("Done running Maven build.")
	return nil
}

// Analyze parses the output of `mvn dependency:list`
func (builder *MavenBuilder) Analyze(m module.Module, allowUnresolved bool) ([]module.Dependency, error) {
	log.Logger.Debugf("Running Maven analysis: %#v %#v", m, allowUnresolved)

	output, _, err := runLogged(m.Dir, builder.MvnCmd, "dependency:tree")
	if err != nil {
		return nil, fmt.Errorf("could not get dependency list from Maven: %s", err.Error())
	}

	// Get dependency tree (rooted at each direct dependency).
	lines := strings.Split(string(output), "\n")
	startRegex := regexp.MustCompile("^\\[INFO\\] --- .*? ---$")
	var depLines []string
	inGraph := false
	for _, line := range lines {
		if startRegex.MatchString(line) {
			if inGraph {
				// Sanity check
				log.Logger.Panicf("Bad graph separation: %s", line)
			}
			inGraph = true
			continue
		}
		if line == "[INFO] " || line == "[INFO] ------------------------------------------------------------------------" {
			inGraph = false
			continue
		}
		if inGraph {
			depLines = append(depLines, line)
		}
	}

	// Parse dependency tree
	var imports []Imported
	root := module.Locator{
		Fetcher:  "root",
		Project:  "root",
		Revision: "root",
	}
	from := module.ImportPath{root}
	depRegex := regexp.MustCompile("^\\[INFO\\] ([ `+\\\\|-]*)([^ `+\\\\|-].+)$")
	locatorRegex := regexp.MustCompile("([^:]+):([^:]+):([^:]*):([^:]+)")
	for _, line := range depLines {
		// Match for context
		depMatches := depRegex.FindStringSubmatch(line)
		depth := len(depMatches[1])
		if depth%3 != 0 {
			// Sanity check
			log.Logger.Panicf("Bad depth: %#v %s %#v", depth, line, depMatches)
		}
		// Parse locator
		locatorMatches := locatorRegex.FindStringSubmatch(depMatches[2])
		locator := module.Locator{
			Fetcher:  "mvn",
			Project:  locatorMatches[1] + ":" + locatorMatches[2],
			Revision: locatorMatches[4],
		}
		// Add to imports
		from = from[:depth/3]
		imports = append(imports, Imported{
			Locator: locator,
			From:    append(module.ImportPath{}, from...),
		})
		from = append(from, locator)
	}
	deps := computeImportPaths(imports)

	log.Logger.Debugf("Done running Maven analysis: %#v", deps)
	return deps, nil
}

// IsBuilt checks whether `mvn dependency:list` produces output.
func (builder *MavenBuilder) IsBuilt(m module.Module, allowUnresolved bool) (bool, error) {
	log.Logger.Debugf("Checking Maven build: %#v %#v", m, allowUnresolved)

	output, _, err := runLogged(m.Dir, builder.MvnCmd, "dependency:list", "-B")
	if err != nil {
		if strings.Index(output, "Could not find artifact") != -1 {
			return false, nil
		}
		return false, err
	}
	isBuilt := output != ""

	log.Logger.Debugf("Done checking Maven build: %#v", isBuilt)
	return isBuilt, nil
}

// IsModule is not implemented
func (builder *MavenBuilder) IsModule(target string) (bool, error) {
	return false, errors.New("IsModule is not implemented for MavenBuilder")
}

// DiscoverModules finds either a root pom.xml file or all pom.xmls in the specified dir
func (builder *MavenBuilder) DiscoverModules(dir string) ([]module.Config, error) {
	_, err := os.Stat(filepath.Join(dir, "pom.xml"))
	if err == nil {
		// Root pom found; parse and return
		artifactName := filepath.Base(filepath.Dir(dir))
		var rootPom POMFile
		if err := parseLoggedWithUnmarshaller(filepath.Join(dir, "pom.xml"), &rootPom, xml.Unmarshal); err == nil {
			if rootPom.Name != "" {
				artifactName = rootPom.Name
			} else if rootPom.ArtifactID != "" {
				artifactName = rootPom.ArtifactID
			}

		}
		return []module.Config{
			{
				Name: artifactName,
				Path: "pom.xml",
				Type: "mvn",
			},
		}, nil
	}

	// No pom in root directory; find and parse all of them
	pomFilePaths, err := doublestar.Glob(filepath.Join(dir, "**", "pom.xml"))
	if err != nil {
		return nil, err
	}
	moduleConfigs := make([]module.Config, len(pomFilePaths))
	for i, path := range pomFilePaths {
		artifactName := filepath.Base(filepath.Dir(dir))
		var artifactPom POMFile
		if err := parseLoggedWithUnmarshaller(path, &artifactPom, xml.Unmarshal); err == nil {
			if artifactPom.Name != "" {
				artifactName = artifactPom.Name
			} else if artifactPom.ArtifactID != "" {
				artifactName = artifactPom.ArtifactID
			}
		}
		path, _ := filepath.Rel(dir, path)
		moduleConfigs[i] = module.Config{
			Name: artifactName,
			Path: path,
			Type: "mvn",
		}
	}

	return moduleConfigs, nil
}
