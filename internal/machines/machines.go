package machines

import (
	"sort"
	"strings"

	"github.com/ubuntu/zsys/internal/config"

	log "github.com/sirupsen/logrus"
	"github.com/ubuntu/zsys/internal/zfs"
	"golang.org/x/xerrors"
)

// Machines hold a zfs system states, with a map of main root system dataset name to a given Machine,
// current machine and nextState if an upgrade has been proceeded.
type Machines struct {
	all               map[string]*Machine
	current           *Machine
	nextState         *State
	allSystemDatasets []zfs.Dataset
	allUsersDatasets  []zfs.Dataset
}

// Machine is a group of Main and its History children statees
type Machine struct {
	// Main machine State
	State
	// History is a map or root system datasets to all its possible State
	History map[string]*State `json:",omitempty"`
}

// State is a finite regroupement of multiple ID and elements corresponding to a bootable machine instance.
type State struct {
	// ID is the path to the root system dataset for this State.
	ID string
	// IsZsys states if we have a zsys system. The other datasets type will be empty otherwise.
	IsZsys bool `json:",omitempty"`
	// SystemDatasets are all datasets that constitues this State (in <pool>/ROOT/ + <pool>/BOOT/)
	SystemDatasets []zfs.Dataset `json:",omitempty"`
	// UserDatasets are all datasets that are attached to the given State (in <pool>/USERDATA/)
	UserDatasets []zfs.Dataset `json:",omitempty"`
	// PersistentDatasets are all datasets that are canmount=on and and not in ROOT, USERDATA or BOOT dataset containers.
	// Those are common between all machines, as persistent (and detected without snapshot information)
	PersistentDatasets []zfs.Dataset `json:",omitempty"`
}

const (
	userdatasetsContainerName = "/userdata/"
)

// sortDataset enables sorting a slice of Dataset elements.
type sortedDataset []zfs.Dataset

func (s sortedDataset) Len() int { return len(s) }
func (s sortedDataset) Less(i, j int) bool {
	// We need snapshots root datasets before snapshot children, count the number of / and order by this.
	subDatasetsI := strings.Count(s[i].Name, "/")
	subDatasetsJ := strings.Count(s[j].Name, "/")
	if subDatasetsI != subDatasetsJ {
		return subDatasetsI < subDatasetsJ
	}

	return s[i].Name < s[j].Name
}
func (s sortedDataset) Swap(i, j int) { s[i], s[j] = s[j], s[i] }

// isChild returns if a dataset d is a child of name.
// An error will mean that the dataset name isn't what we expected it to be.
func isChild(name string, d zfs.Dataset) (bool, error) {
	names := strings.Split(name, "@")
	var err error
	switch len(names) {
	// direct system or clone child
	case 1:
		if strings.HasPrefix(d.Name, names[0]+"/") && !strings.Contains(d.Name, "@") {
			return true, nil
		}
	// snapshot child
	case 2:
		if strings.HasPrefix(d.Name, names[0]+"/") && strings.HasSuffix(d.Name, "@"+names[1]) {
			return true, nil
		}
	default:
		err = xerrors.Errorf("unexpected number of @ in dataset name %q", d.Name)
	}
	return false, err
}

// resolveOrigin iterates over each datasets up to their true origin and replaces them.
// This is only done for /, /boot* and user datasets for performance reasons.
func resolveOrigin(datasets []zfs.Dataset) map[string]*string {
	r := make(map[string]*string)
	for _, curDataset := range datasets {
		if curDataset.Mountpoint != "/" && !strings.HasPrefix(curDataset.Mountpoint, "/boot") &&
			!strings.Contains(strings.ToLower(curDataset.Name), userdatasetsContainerName) {
			continue
		}

		// copy to a local variable so that they don't all use the same address
		origin := curDataset.Origin
		if curDataset.IsSnapshot {
			origin = curDataset.Name

		}
		r[curDataset.Name] = &origin

		if *r[curDataset.Name] == "" && !curDataset.IsSnapshot {
			continue
		}

		curOrig := r[curDataset.Name]
	nextOrigin:
		for {
			// origin for a clone points to a snapshot, points directly to the originating file system datasets to prevent a hop
			if j := strings.LastIndex(*curOrig, "@"); j > 0 {
				*curOrig = (*curOrig)[:j]
			}

			originStart := *curOrig
			for _, d := range datasets {
				if *curOrig != d.Name {
					continue
				}
				if d.Origin != "" {
					*curOrig = d.Origin
					break
				}
				break nextOrigin
			}
			if originStart == *curOrig {
				log.Warningf("didn't find origin %q for %q matching any dataset", *curOrig, curDataset.Name)
				delete(r, curDataset.Name)
				break
			}
		}
	}
	return r
}

// New detects and generate machines elems
func New(ds []zfs.Dataset, cmdline string) Machines {
	machines := Machines{
		all: make(map[string]*Machine),
	}

	// We are going to transform the origin of datasets, get a copy first
	datasets := make([]zfs.Dataset, len(ds))
	copy(datasets, ds)

	// Sort datasets so that children datasets are after their parents.
	sortedDataset := sortedDataset(datasets)
	sort.Sort(sortedDataset)

	// Resolve out to its root origin for /, /boot* and user datasets
	origins := resolveOrigin([]zfs.Dataset(sortedDataset))

	var boots, persistents []zfs.Dataset
	// First, handle system datasets (active for each machine and history)
nextDataset:
	for _, d := range sortedDataset {
		// Register all zsys non cloned mountable / to a new machine
		if d.Mountpoint == "/" && *origins[d.Name] == "" && d.CanMount != "off" {
			m := Machine{
				State: State{
					ID:             d.Name,
					IsZsys:         d.BootFS,
					SystemDatasets: []zfs.Dataset{d},
				},
				History: make(map[string]*State),
			}
			machines.all[d.Name] = &m
			continue
		}

		// Check for children, clones and snapshots
		for _, m := range machines.all {
			// Direct main machine state children
			if ok, err := isChild(m.ID, d); err != nil {
				log.Warningf("ignoring %q as couldn't assert if it's a child: "+config.ErrorFormat, d.Name, err)
			} else if ok {
				m.SystemDatasets = append(m.SystemDatasets, d)
				continue nextDataset
			}

			// Clones or snapshot root dataset (origins points to origin dataset)
			if d.Mountpoint == "/" && *origins[d.Name] == m.ID && d.CanMount != "off" {
				m.History[d.Name] = &State{
					ID:             d.Name,
					IsZsys:         d.BootFS,
					SystemDatasets: []zfs.Dataset{d},
				}
				continue nextDataset
			}

			// Clones or snapshot children
			for _, h := range m.History {
				if ok, err := isChild(h.ID, d); err != nil {
					log.Warningf("ignoring %q as couldn't assert if it's a child: "+config.ErrorFormat, d.Name, err)
				} else if ok {
					h.SystemDatasets = append(h.SystemDatasets, d)
					continue nextDataset
				}
			}
		}

		// Starting from now, there is no children of system datasets

		// Extract boot datasets if any. We can't attach them directly with machines as if they are on another pool,
		// the machine is not necessiraly loaded yet.
		if strings.HasPrefix(d.Mountpoint, "/boot") {
			boots = append(boots, d)
			continue
		}

		// Extract zsys user datasets if any. We can't attach them directly with machines as if they are on another pool,
		// the machine is not necessiraly loaded yet.
		if strings.Contains(strings.ToLower(d.Name), userdatasetsContainerName) {
			machines.allUsersDatasets = append(machines.allUsersDatasets, d)
			continue
		}

		// At this point, it's either non zfs system or persistent dataset. Filters out canmount != "on" as nothing
		// will mount them.
		if d.CanMount != "on" {
			log.Debugf("ignoring %q: not a boot, user or system datasets and canmount isn't on", d.Name)
			continue
		}

		// should be persistent datasets
		persistents = append(persistents)
	}

	// Attach to machine zsys boots and userdata non persisent datasets per machines before attaching persistents.
	// Same with children and history datasets.
	for _, m := range machines.all {
		e := strings.Split(m.ID, "/")
		// machineDatasetID is the main State dataset ID.
		machineDatasetID := e[len(e)-1]

		machines.allSystemDatasets = append(machines.allSystemDatasets, m.SystemDatasets...)

		// Boot datasets
		var bootsDataset []zfs.Dataset
		for _, d := range boots {
			// Matching base dataset name or subdataset of it.
			if strings.HasSuffix(d.Name, "/"+machineDatasetID) || strings.Contains(d.Name, "/"+machineDatasetID+"/") {
				bootsDataset = append(bootsDataset, d)
			}
		}
		m.SystemDatasets = append(m.SystemDatasets, bootsDataset...)

		// Userdata datasets. Don't base on machineID name as it's a tag on the dataset (the same userdataset can be
		// linked to multiple clones and systems).
		var userDatasets []zfs.Dataset
		for _, d := range machines.allUsersDatasets {
			// Only match datasets corresponding to the linked bootfs datasets (string slice separated by :)
			for _, bootfsDataset := range strings.Split(d.BootfsDatasets, ":") {
				if bootfsDataset == m.ID || strings.HasPrefix(d.BootfsDatasets, m.ID+"/") {
					userDatasets = append(userDatasets, d)
				}
			}
		}
		m.UserDatasets = append(m.UserDatasets, userDatasets...)

		// Persistent datasets
		m.PersistentDatasets = persistents

		// Handle history now
		for lu, h := range m.History {
			e := strings.Split(h.ID, "/")
			// stateDatasetID may contain @snapshot, which we need to strip to test the suffix
			stateDatasetID := e[len(e)-1]
			var snapshot string
			if j := strings.LastIndex(stateDatasetID, "@"); j > 0 {
				snapshot = stateDatasetID[j+1:]
			}

			machines.allSystemDatasets = append(machines.allSystemDatasets, h.SystemDatasets...)

			// Boot datasets
			var bootsDataset []zfs.Dataset
			for _, d := range boots {
				if snapshot != "" {
					// Snapshots are not necessarily with a dataset ID maching its parent of dataset promotions, just match
					// its name.
					if strings.HasSuffix(d.Name, "@"+snapshot) {
						bootsDataset = append(bootsDataset, d)
						continue
					}
				}
				// For clones just match the base datasetname or its children.
				if strings.HasSuffix(d.Name, stateDatasetID) || strings.Contains(d.Name, "/"+stateDatasetID+"/") {
					bootsDataset = append(bootsDataset, d)
				}
			}
			h.SystemDatasets = append(h.SystemDatasets, bootsDataset...)

			// Userdata datasets. Don't base on machineID name as it's a tag on the dataset (the same userdataset can be
			// linked to multiple clones and systems).
			var userDatasets []zfs.Dataset
			for _, d := range machines.allUsersDatasets {
				if snapshot != "" {
					// Snapshots wo'nt match dataset ID maching its system dataset as multiple system datasets can link
					// to the same user dataset. Use only snapshot name.
					if strings.HasSuffix(d.Name, "@"+snapshot) {
						userDatasets = append(userDatasets, d)
						continue
					}
				}
				// For clones, proceed as with main system:
				// Only match datasets corresponding to the linked bootfs datasets (string slice separated by :)
				for _, bootfsDataset := range strings.Split(d.BootfsDatasets, ":") {
					if bootfsDataset == h.ID || strings.HasPrefix(d.BootfsDatasets, h.ID+"/") {
						userDatasets = append(userDatasets, d)
					}
				}
			}
			h.UserDatasets = append(h.UserDatasets, userDatasets...)

			// Persistent datasets
			h.PersistentDatasets = persistents

			m.History[lu] = h
		}
	}

	root, _ := parseCmdLine(cmdline)
	m, _ := machines.findFromRoot(root)
	machines.current = m

	return machines
}

// findFromRoot returns the active machine and state if any.
// If rootName is a snapshot, it fallbacks to current mounted root dataset
func (machines *Machines) findFromRoot(rootName string) (*Machine, *State) {
	// Not a zfs system
	if rootName == "" {
		return nil, nil
	}

	// Fast path: if rootName is already a main dataset state
	if m, exists := machines.all[rootName]; exists {
		return m, &m.State
	}

	var fromSnapshot bool
	if strings.Contains(rootName, "@") {
		fromSnapshot = true
	}

	// We know that our desired target is a history one
	for _, m := range machines.all {
		// Only match on names as we booted an existing clone directly.
		if !fromSnapshot {
			if h, ok := m.History[rootName]; ok {
				return m, h
			}
			continue
		}

		// We have a snapshot, we need to find the corresponding mounted main dataset on /.
		for _, h := range m.History {
			if h.SystemDatasets[0].Mounted && h.SystemDatasets[0].Mountpoint == "/" {
				return m, h
			}
		}
	}

	return nil, nil
}
