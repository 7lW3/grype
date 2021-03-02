package pkg

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/mitchellh/go-homedir"

	"github.com/anchore/grype/grype/cpe"
	"github.com/anchore/syft/syft/distro"
	"github.com/anchore/syft/syft/pkg"
	syftJson "github.com/anchore/syft/syft/presenter/json"
	"github.com/anchore/syft/syft/source"
)

// syftJSONProvider extracts the necessary package and package context from syft JSON output. Note that this process carves out
// only the necessary data needed and does not require unmarshalling the entire syft JSON data shape so this function is somewhat
// resilient to multiple syft JSON schemas (to a degree).
// TODO: add version detection and multi-parser support (when needed in the future)
func syftJSONProvider(config providerConfig) ([]Package, Context, error) {
	reader, err := getSyftJSON(config)
	if err != nil {
		return nil, Context{}, err
	}

	return parseSyftJSON(reader)
}

func getSyftJSON(config providerConfig) (io.Reader, error) {
	if config.reader != nil {
		// the caller has explicitly indicated to use the given reader as input
		return config.reader, nil
	}

	if explicitlySpecifyingSBOM(config.userInput) {
		filepath := strings.TrimPrefix(config.userInput, "sbom:")

		sbom, err := openSbom(filepath)
		if err != nil {
			return nil, fmt.Errorf("unable to use specified SBOM: %w", err)
		}

		return sbom, nil
	}

	// as a last resort, see if the raw user input specified an SBOM file
	sbom, err := openSbom(config.userInput)
	if err == nil {
		return sbom, nil
	}

	// no usable SBOM is available
	return nil, errDoesNotProvide
}

func openSbom(path string) (*os.File, error) {
	expandedPath, err := homedir.Expand(path)
	if err != nil {
		return nil, fmt.Errorf("unable to open SBOM: %w", err)
	}

	sbom, err := os.Open(expandedPath)
	if err != nil {
		return nil, fmt.Errorf("unable to open SBOM: %w", err)
	}

	return sbom, nil
}

func explicitlySpecifyingSBOM(userInput string) bool {
	return strings.HasPrefix(userInput, "sbom:")
}

// partialSyftDoc is the final package shape for a select elements from a syft JSON document.
type partialSyftDoc struct {
	Source    syftJson.Source       `json:"source"`
	Artifacts []partialSyftPackage  `json:"artifacts"`
	Distro    syftJson.Distribution `json:"distro"`
}

// partialSyftPackage is the final package shape for a select elements from a syft JSON package.
type partialSyftPackage struct {
	packageBasicMetadata
	packageCustomMetadata
}

// packageBasicMetadata contains non-ambiguous values (type-wise) from pkg.Package.
type packageBasicMetadata struct {
	Name      string            `json:"name"`
	Version   string            `json:"version"`
	Type      pkg.Type          `json:"type"`
	Locations []source.Location `json:"locations"`
	Licenses  []string          `json:"licenses"`
	Language  pkg.Language      `json:"language"`
	CPEs      []string          `json:"cpes"`
	PURL      string            `json:"purl"`
}

// packageCustomMetadata contains ambiguous values (type-wise) from pkg.Package.
type packageCustomMetadata struct {
	MetadataType pkg.MetadataType `json:"metadataType"`
	Metadata     interface{}      `json:"metadata"`
}

// packageMetadataUnpacker is all values needed from Package to disambiguate ambiguous fields during json unmarshaling.
type packageMetadataUnpacker struct {
	MetadataType pkg.MetadataType `json:"metadataType"`
	Metadata     json.RawMessage  `json:"metadata"`
}

// partialSyftJavaMetadata encapsulates all Java ecosystem metadata for a package as well as an (optional) parent relationship.
type partialSyftJavaMetadata struct {
	VirtualPath   string                    `json:"virtualPath"`
	Manifest      *partialSyftJavaManifest  `mapstructure:"Manifest" json:"manifest,omitempty"`
	PomProperties *partialSyftPomProperties `mapstructure:"PomProperties" json:"pomProperties,omitempty"`
}

// partialSyftPomProperties represents the fields of interest extracted from a Java archive's pom.xml file.
type partialSyftPomProperties struct {
	GroupID    string `mapstructure:"groupId" json:"groupId"`
	ArtifactID string `mapstructure:"artifactId" json:"artifactId"`
}

// partialSyftJavaManifest represents the fields of interest extracted from a Java archive's META-INF/MANIFEST.MF file.
type partialSyftJavaManifest struct {
	Main map[string]string `json:"main,omitempty"`
}

// String returns the stringer representation for a syft package.
func (p partialSyftPackage) String() string {
	return fmt.Sprintf("Pkg(type=%s, name=%s, version=%s)", p.Type, p.Name, p.Version)
}

// UnmarshalJSON is a custom unmarshaller for handling basic values and values with ambiguous types.
func (p *partialSyftPackage) UnmarshalJSON(b []byte) error {
	var basic packageBasicMetadata
	if err := json.Unmarshal(b, &basic); err != nil {
		return err
	}
	p.packageBasicMetadata = basic

	var unpacker packageMetadataUnpacker
	if err := json.Unmarshal(b, &unpacker); err != nil {
		return err
	}

	p.MetadataType = unpacker.MetadataType

	switch p.MetadataType {
	case pkg.RpmdbMetadataType:
		var payload RpmdbMetadata
		if err := json.Unmarshal(unpacker.Metadata, &payload); err != nil {
			return err
		}
		p.Metadata = payload
	case pkg.DpkgMetadataType:
		var payload DpkgMetadata
		if err := json.Unmarshal(unpacker.Metadata, &payload); err != nil {
			return err
		}
		p.Metadata = payload
	case pkg.JavaMetadataType:
		var partialPayload partialSyftJavaMetadata
		if err := json.Unmarshal(unpacker.Metadata, &partialPayload); err != nil {
			return err
		}

		var artifact, group, name string
		if partialPayload.PomProperties != nil {
			artifact = partialPayload.PomProperties.ArtifactID
			group = partialPayload.PomProperties.GroupID
		}

		if partialPayload.Manifest != nil {
			if n, ok := partialPayload.Manifest.Main["Name"]; ok {
				name = n
			}
		}

		p.Metadata = JavaMetadata{
			PomArtifactID: artifact,
			PomGroupID:    group,
			ManifestName:  name,
		}
	}

	return nil
}

// parseSyftJSON attempts to loosely parse the available JSON for only the fields needed, not the exact syft JSON shape.
// This allows for some resiliency as the syft document shape changes over time (but not fool-proof).
func parseSyftJSON(reader io.Reader) ([]Package, Context, error) {
	var doc partialSyftDoc
	decoder := json.NewDecoder(reader)
	if err := decoder.Decode(&doc); err != nil {
		return nil, Context{}, errDoesNotProvide
	}

	var packages = make([]Package, len(doc.Artifacts))
	for i, a := range doc.Artifacts {
		cpes, err := cpe.NewSlice(a.CPEs...)
		if err != nil {
			return nil, Context{}, err
		}

		packages[i] = Package{
			id:        ID(i),
			Name:      a.Name,
			Version:   a.Version,
			Locations: a.Locations,
			Language:  a.Language,
			Licenses:  a.Licenses,
			Type:      a.Type,
			CPEs:      cpes,
			PURL:      a.PURL,
			Metadata:  a.Metadata,
		}
	}

	var theDistro *distro.Distro
	if doc.Distro.Name != "" {
		d, err := distro.NewDistro(distro.Type(doc.Distro.Name), doc.Distro.Version, doc.Distro.IDLike)
		if err != nil {
			return nil, Context{}, err
		}
		theDistro = &d
	}

	srcMetadata := doc.Source.ToSourceMetadata()

	return packages, Context{
		Source: &srcMetadata,
		Distro: theDistro,
	}, nil
}
