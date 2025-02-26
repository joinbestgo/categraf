package gnmi

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"path"
	"sort"
	"strconv"
	"strings"
	"time"

	gnmiLib "github.com/openconfig/gnmi/proto/gnmi"
	gnmiExt "github.com/openconfig/gnmi/proto/gnmi_ext"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	jnprHeader "flashcat.cloud/categraf/inputs/gnmi/extensions/jnpr_gnmi_extention"
	"flashcat.cloud/categraf/pkg/choice"
	"flashcat.cloud/categraf/types"
)

const eidJuniperTelemetryHeader = 1

type handler struct {
	address             string
	aliases             map[*pathInfo]string
	tagsubs             []TagSubscription
	subs                []Subscription
	maxMsgSize          int
	emptyNameWarnShown  bool
	vendorExt           []string
	tagStore            *tagStore
	trace               bool
	canonicalFieldNames bool
	trimSlash           bool
	guessPathTag        bool

	sourceTag string
}

// SubscribeGNMI and extract telemetry data
func (h *handler) subscribeGNMI(ctx context.Context, slist *types.SampleList, tlscfg *tls.Config, request *gnmiLib.SubscribeRequest) error {
	var creds credentials.TransportCredentials
	if tlscfg != nil {
		creds = credentials.NewTLS(tlscfg)
	} else {
		creds = insecure.NewCredentials()
	}
	opts := []grpc.DialOption{
		grpc.WithTransportCredentials(creds),
	}

	if h.maxMsgSize > 0 {
		opts = append(opts, grpc.WithDefaultCallOptions(
			grpc.MaxCallRecvMsgSize(h.maxMsgSize),
		))
	}

	client, err := grpc.DialContext(ctx, h.address, opts...)
	if err != nil {
		return fmt.Errorf("failed to dial: %w", err)
	}
	defer client.Close()

	subscribeClient, err := gnmiLib.NewGNMIClient(client).Subscribe(ctx)
	if err != nil {
		return fmt.Errorf("failed to setup subscription: %w", err)
	}

	// If io.EOF is returned, the stream may have ended and stream status
	// can be determined by calling Recv.
	if err := subscribeClient.Send(request); err != nil && !errors.Is(err, io.EOF) {
		return fmt.Errorf("failed to send subscription request: %w", err)
	}

	log.Printf("Connection to gNMI device %s established", h.address)

	// Used to report the status of the TCP connection to the device. If the
	// GNMI connection goes down, but TCP is still up this will still report
	// connected until the TCP connection times out.
	// connectStat := selfstat.Register("gnmi", "grpc_connection_status", map[string]string{"source": h.address})
	// connectStat.Set(1)

	defer log.Printf("Connection to gNMI device %s closed", h.address)
	for ctx.Err() == nil {
		var reply *gnmiLib.SubscribeResponse
		if reply, err = subscribeClient.Recv(); err != nil {
			if !errors.Is(err, io.EOF) && ctx.Err() == nil {
				// connectStat.Set(0)
				return fmt.Errorf("aborted gNMI subscription: %w", err)
			}
			break
		}

		if h.trace {
			buf, err := protojson.Marshal(reply)
			if err != nil {
				log.Printf("Marshal failed: %v", err)
			} else {
				t := reply.GetUpdate().GetTimestamp()
				log.Printf("Got update_%v: %s", t, string(buf))
			}
		}
		if response, ok := reply.Response.(*gnmiLib.SubscribeResponse_Update); ok {
			h.handleSubscribeResponseUpdate(slist, response, reply.GetExtension())
		}
	}

	// connectStat.Set(0)
	return nil
}

// Handle SubscribeResponse_Update message from gNMI and parse contained telemetry data
func (h *handler) handleSubscribeResponseUpdate(slist *types.SampleList, response *gnmiLib.SubscribeResponse_Update, extension []*gnmiExt.Extension) {
	timestamp := time.Unix(0, response.Update.Timestamp)

	// Extract tags from potential extension in the update notification
	headerTags := make(map[string]string)
	for _, ext := range extension {
		currentExt := ext.GetRegisteredExt().Msg
		if currentExt == nil {
			break
		}

		switch ext.GetRegisteredExt().Id {
		case eidJuniperTelemetryHeader:
			// Juniper Header extention
			// Decode it only if user requested it
			if choice.Contains("juniper_header", h.vendorExt) {
				juniperHeader := &jnprHeader.GnmiJuniperTelemetryHeaderExtension{}
				if err := proto.Unmarshal(currentExt, juniperHeader); err != nil {
					log.Printf("unmarshal gnmi Juniper Header extension failed: %v", err)
				} else {
					// Add only relevant Tags from the Juniper Header extension.
					// These are required for aggregation
					headerTags["component_id"] = strconv.FormatUint(uint64(juniperHeader.GetComponentId()), 10)
					headerTags["component"] = juniperHeader.GetComponent()
					headerTags["sub_component_id"] = strconv.FormatUint(uint64(juniperHeader.GetSubComponentId()), 10)
				}
			}
		default:
			continue
		}
	}

	// Extract the path part valid for the whole set of updates if any
	prefix := newInfoFromPath(response.Update.Prefix)

	// Add info to the tags
	headerTags[h.sourceTag], _, _ = net.SplitHostPort(h.address)
	if !prefix.empty() {
		headerTags["path"] = prefix.String()
	}

	// Process and remove tag-updates from the response first so we can
	// add all available tags to the metrics later.
	var valueFields []updateField
	for _, update := range response.Update.Update {
		fullPath := prefix.append(update.Path)
		fields, err := newFieldsFromUpdate(fullPath, update)
		if err != nil {
			log.Printf("Processing update %v failed: %v", update, err)
		}

		// Prepare tags from prefix
		tags := make(map[string]string, len(headerTags))
		for key, val := range headerTags {
			tags[key] = val
		}
		for key, val := range fullPath.Tags() {
			tags[key] = val
		}

		// TODO: Handle each field individually to allow in-JSON tags
		var tagUpdate bool
		for _, tagSub := range h.tagsubs {
			if !fullPath.equalsPathNoKeys(tagSub.fullPath) {
				continue
			}
			log.Printf("Tag-subscription update for %q: %+v", tagSub.Name, update)
			if err := h.tagStore.insert(tagSub, fullPath, fields, tags); err != nil {
				log.Printf("E! Inserting tag failed: %v", err)
			}
			tagUpdate = true
			break
		}
		if !tagUpdate {
			valueFields = append(valueFields, fields...)
		}
	}

	// Some devices do not provide a prefix, so do some guesswork based
	// on the paths of the fields
	if headerTags["path"] == "" && h.guessPathTag {
		if prefixPath := guessPrefixFromUpdate(valueFields); prefixPath != "" {
			headerTags["path"] = prefixPath
		}
	}

	// Parse individual update message and create measurements
	for _, field := range valueFields {
		// Prepare tags from prefix
		fieldTags := field.path.Tags()
		tags := make(map[string]string, len(headerTags)+len(fieldTags))
		for key, val := range headerTags {
			tags[key] = val
		}
		for key, val := range fieldTags {
			tags[key] = val
		}

		// Add the tags derived via tag-subscriptions
		for k, v := range h.tagStore.lookup(field.path, tags) {
			tags[k] = v
		}

		// Lookup alias for the metric
		aliasPath, name := h.lookupAlias(field.path)
		if name == "" {
			log.Printf("No measurement alias for gNMI path: %s", field.path)
			if !h.emptyNameWarnShown {
				log.Printf(emptyNameWarning, response.Update)
				h.emptyNameWarnShown = true
			}
		}

		// Group metrics
		fieldPath := field.path.String()
		key := strings.ReplaceAll(fieldPath, "-", "_")
		if h.canonicalFieldNames {
			// Strip the origin is any for the field names
			if parts := strings.SplitN(key, ":", 2); len(parts) == 2 {
				key = parts[1]
			}
		} else {
			if len(aliasPath) < len(key) && len(aliasPath) != 0 {
				// This may not be an exact prefix, due to naming style
				// conversion on the key.
				key = key[len(aliasPath)+1:]
			} else if len(aliasPath) >= len(key) {
				// Otherwise use the last path element as the field key.
				key = path.Base(key)
			}
		}
		if h.trimSlash {
			key = strings.TrimLeft(key, "/.")
		}
		if key == "" {
			log.Printf("E! Invalid empty path %q with alias %q", fieldPath, aliasPath)
			continue
		}
		prefix := inputName
		if strings.HasPrefix(name, inputName) {
			prefix = ""
		}
		value := field.value
		disableConcatenating := false
		base := field.path.String()
		for _, sub := range h.subs {
			subPath := sub.Path
			if len(strings.Split(base, ":")) >= 2 {
				// origin is not null
				if sub.Origin != "" {
					subPath = sub.Origin + ":" + sub.Path
				}
			}
			if strings.HasPrefix(base, subPath) {
				// field.path => origin:path
				// sub.path => path
				disableConcatenating = sub.DisableConcatenation
				switch sub.Conversion {
				case "ieeefloat32":
					switch val := field.value.(type) {
					case string:
						v, err := string2float32(val)
						if err != nil {
						}

						value = v
					case []uint8:
						v, err := bytes2float32(val)
						if err != nil {

						}
						value = v
					}
				}
			}
		}
		if !disableConcatenating {
			name = name + "_" + key
		}

		sample := types.NewSample(prefix, name, value, tags).SetTime(timestamp)
		slist.PushFront(sample)
	}

}

// Try to find the alias for the given path
type aliasCandidate struct {
	path, alias string
}

func (h *handler) lookupAlias(info *pathInfo) (aliasPath, alias string) {
	candidates := make([]aliasCandidate, 0)
	for i, a := range h.aliases {
		if !i.isSubPathOf(info) {
			continue
		}
		candidates = append(candidates, aliasCandidate{i.String(), a})
	}
	if len(candidates) == 0 {
		return "", ""
	}

	// Reverse sort the candidates by path length so we can use the longest match
	sort.SliceStable(candidates, func(i, j int) bool {
		return len(candidates[i].path) > len(candidates[j].path)
	})

	return candidates[0].path, candidates[0].alias
}

func guessPrefixFromUpdate(fields []updateField) string {
	if len(fields) == 0 {
		return ""
	}
	if len(fields) == 1 {
		dir, _ := fields[0].path.split()
		return dir
	}
	commonPath := &pathInfo{
		origin:   fields[0].path.origin,
		segments: append([]string{}, fields[0].path.segments...),
	}
	for _, f := range fields[1:] {
		commonPath.keepCommonPart(f.path)
	}
	if commonPath.empty() {
		return ""
	}
	return commonPath.String()
}
