package dav

import (
	"encoding/xml"
	"io"
	"net/url"
	"strings"
)

// parsePropUpdate decodes a PROPPATCH body into a flat list of property sets.
//
// Both <set> and <remove> are flattened: storing an empty value is how "remove"
// is represented, which is what every client understands and keeps the storage
// model to a single map.
func parsePropUpdate(body io.Reader) ([]deadProp, error) {
	raw, err := io.ReadAll(io.LimitReader(body, 1<<20))
	if err != nil {
		return nil, err
	}
	if len(strings.TrimSpace(string(raw))) == 0 {
		return nil, nil
	}

	// The nesting is deliberate: <propertyupdate> holds <set>/<remove>, each of
	// which holds a <prop>. Encoding that as one flat struct with a "prop>"
	// selector does not work — the ">" form is only valid on the outermost
	// element, so it silently yields zero operations.
	type prop struct {
		XMLName xml.Name
		Value   string `xml:",chardata"`
	}
	type propContainer struct {
		Props []prop `xml:",any"`
	}
	type operation struct {
		XMLName xml.Name
		Prop    propContainer `xml:"prop"`
	}
	var doc struct {
		XMLName xml.Name    `xml:"propertyupdate"`
		Ops     []operation `xml:",any"`
	}
	if err := xml.Unmarshal(raw, &doc); err != nil {
		return nil, err
	}

	var out []deadProp
	for _, op := range doc.Ops {
		switch op.XMLName.Local {
		case "set":
			for _, p := range op.Prop.Props {
				out = append(out, deadProp{Space: p.XMLName.Space, Name: p.XMLName.Local, Value: p.Value})
			}
		case "remove":
			for _, p := range op.Prop.Props {
				out = append(out, deadProp{Space: p.XMLName.Space, Name: p.XMLName.Local, Value: ""})
			}
		}
	}
	return out, nil
}

// parseDestination extracts the path from a Destination header.
//
// The header is an absolute URL. Only the path is used, and a foreign host is
// rejected: honouring it would turn COPY into an SSRF probe against whatever the
// client names.
func parseDestination(dest string) (string, error) {
	u, err := url.Parse(dest)
	if err != nil {
		return "", errBadDestination
	}
	if u.Scheme == "" && u.Host == "" {
		// Some clients send a bare path.
		if u.Path == "" {
			return "", errBadDestination
		}
		return u.Path, nil
	}
	if u.Path == "" {
		return "", errBadDestination
	}
	return u.Path, nil
}

// destinationHostMatches reports whether a Destination URL points at this server.
//
// It is checked by the caller against the configured public URL: accepting any
// host would let a client redirect a server-side copy to an arbitrary destination.
func destinationHostMatches(dest, publicURL string) bool {
	u, err := url.Parse(dest)
	if err != nil {
		return false
	}
	if u.Host == "" {
		return true // bare path, always local
	}
	pub, err := url.Parse(publicURL)
	if err != nil {
		return false
	}
	return strings.EqualFold(u.Host, pub.Host)
}
