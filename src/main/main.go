package main

import (
	"bytes"
	"crypto/sha1"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	log "github.com/Sirupsen/logrus"
	"gopkg.in/alecthomas/kingpin.v2"
	"io/ioutil"
	"math"
	"mime/multipart"
	"net/http"
	"net/url"
	"qBT"
	"strconv"
	"strings"
	"time"
	"transmission"
)

var (
	verbose = kingpin.Flag("verbose", "Enable debug output").Short('v').Bool()
	apiAddr = kingpin.Flag("api-addr", "qBittorrent API address").Short('r').Default("http://localhost:8080/").String()
	port    = kingpin.Flag("port", "Transmission RPC port").Short('p').Default("9091").Int()
)

var deprecatedFields = []string{
	"announceResponse",
	"seeders",
	"leechers",
	"downloadLimitMode",
	"uploadLimitMode",
	"nextAnnounceTime",
}

var qBTConn qBT.Connection

func IsFieldDeprecated(field string) bool {
	for _, value := range deprecatedFields {
		if value == field {
			return true
		}
	}
	return false
}

func parseIDsArgument(args *json.RawMessage) []int {
	allIds := parseIDsField(args)
	filtered := make([]int, 0)
	for _, id := range allIds {
		if qBTConn.GetHashForId(id) != "" {
			filtered = append(filtered, id)
		}
	}
	return filtered
}

func parseIDsField(args *json.RawMessage) []int {
	if args == nil {
		log.Debug("No IDs provided")
		result := make([]int, qBTConn.GetHashNum())
		for i := 0; i < qBTConn.GetHashNum(); i++ {
			result[i] = i + 1
		}
		return result
	}

	var ids interface{}
	err := json.Unmarshal(*args, &ids)
	Check(err)

	switch ids := ids.(type) {
	case float64:
		log.Debug("Query a single ID")
		return []int{int(ids)}
	case []interface{}:
		log.Debug("Query an ID list of length ", len(ids))
		result := make([]int, len(ids))
		for i, value := range ids {
			switch id := value.(type) {
			case float64:
				result[i] = int(id)
			case string:
				result[i], err = qBTConn.GetIdOfHash(id)
				Check(err)
			}
		}
		return result
	case string:
		log.Debug("Query recently-active") // TODO
		result := make([]int, qBTConn.GetHashNum())
		for i := 0; i < qBTConn.GetHashNum(); i++ {
			result[i] = i + 1
		}
		return result
	default:
		log.Error("Unknown action")
		return []int{}
	}
}

func parseActionArgument(args json.RawMessage) []string {
	var req struct {
		Ids json.RawMessage
	}
	err := json.Unmarshal(args, &req)
	Check(err)

	ids := parseIDsArgument(&req.Ids)
	hashes := make([]string, len(ids))
	for i, value := range ids {
		hashes[i] = qBTConn.GetHashForId(value)
	}
	return hashes
}

func MapTorrentList(dst JsonMap, torrentsList []qBT.TorrentsList, id int) {
	var src qBT.TorrentsList
	for _, value := range torrentsList {
		if value.Hash == qBTConn.GetHashForId(id) {
			src = value
		}
	}

	for key, value := range transmission.TorrentGetBase {
		dst[key] = value
	}
	dst["hashString"] = src.Hash
	dst["name"] = src.Name
	dst["recheckProgress"] = src.Progress
	dst["sizeWhenDone"] = src.Size
	dst["rateDownload"] = src.Dlspeed
	dst["rateUpload"] = src.Upspeed
	dst["uploadRatio"] = src.Ratio
	dst["eta"] = src.Eta
	switch src.State {
	case "pausedUP", "pausedDL":
		dst["status"] = 0 // TR_STATUS_STOPPED
	case "checkingUP", "checkingDL":
		dst["status"] = 2 // TR_STATUS_CHECK
	case "queuedDL":
		dst["status"] = 3 // TR_STATUS_DOWNLOAD_WAIT
	case "downloading", "stalledDL":
		dst["status"] = 4 // TR_STATUS_DOWNLOAD
	case "queuedUP":
		dst["status"] = 5 // TR_STATUS_SEED_WAIT
	case "uploading", "stalledUP":
		dst["status"] = 6 // TR_STATUS_SEED
	case "error":
		dst["status"] = 0 // TR_STATUS_STOPPED
	default:
		dst["status"] = 0 // TR_STATUS_STOPPED
	}
	if src.State == "error" {
		dst["error"] = 3 // TR_STAT_LOCAL_ERROR
	} else {
		dst["error"] = 0 // TR_STAT_OK
	}
	switch src.State {
	case "stalledDL", "stalledUP":
		dst["isStalled"] = true
	default:
		dst["isStalled"] = false
	}
	dst["percentDone"] = src.Progress
	dst["peersGettingFromUs"] = src.Num_leechs
	dst["peersSendingToUs"] = src.Num_seeds
	dst["leftUntilDone"] = float64(src.Size) * (1 - src.Progress)
	dst["desiredAvailable"] = float64(src.Size) * (1 - src.Progress) // TODO
	dst["haveUnchecked"] = 0                                         // TODO
}

func MakePiecesBitArray(total, have int) string {
	if (total < 0) || (have < 0) {
		return "" // Empty array
	}
	arrLen := uint(math.Ceil(float64(total) / 8))
	arr := make([]byte, arrLen)

	fullBytes := uint(math.Floor(float64(have) / 8))
	for i := uint(0); i < fullBytes; i++ {
		arr[i] = math.MaxUint8
	}
	for i := uint(0); i < (uint(have) - fullBytes*8); i++ {
		arr[fullBytes] |= 128 >> i
	}

	return base64.StdEncoding.EncodeToString(arr)
}

func MapPropsGeneral(dst JsonMap, propGeneral qBT.PropertiesGeneral) {
	dst["downloadDir"] = propGeneral.Save_path
	dst["pieceSize"] = propGeneral.Piece_size
	dst["pieceCount"] = propGeneral.Pieces_num
	dst["addedDate"] = propGeneral.Addition_date
	dst["startDate"] = propGeneral.Addition_date // TODO
	dst["comment"] = propGeneral.Comment
	dst["dateCreated"] = propGeneral.Creation_date
	dst["creator"] = propGeneral.Created_by
	dst["doneDate"] = propGeneral.Completion_date
	dst["totalSize"] = propGeneral.Total_size
	dst["haveValid"] = propGeneral.Piece_size * propGeneral.Pieces_have
	dst["downloadedEver"] = propGeneral.Total_downloaded
	dst["uploadedEver"] = propGeneral.Total_uploaded
	dst["pieces"] = MakePiecesBitArray(propGeneral.Pieces_num, propGeneral.Pieces_have)
	dst["peersConnected"] = propGeneral.Peers
	dst["corruptEver"] = propGeneral.Total_wasted

	if propGeneral.Up_limit >= 0 {
		dst["uploadLimited"] = true
		dst["uploadLimit"] = propGeneral.Up_limit
	} else {
		dst["uploadLimited"] = false
		dst["uploadLimit"] = 0
	}

	if propGeneral.Dl_limit >= 0 {
		dst["downloadLimited"] = true
		dst["downloadLimit"] = propGeneral.Dl_limit // TODO: Kb/s?
	} else {
		dst["downloadLimited"] = false
		dst["downloadLimit"] = 0
	}

	dst["maxConnectedPeers"] = propGeneral.Nb_connections_limit
	dst["peer-limit"] = propGeneral.Nb_connections_limit // TODO: What's it?
}

func MapPropsTrackers(dst JsonMap, trackers []qBT.PropertiesTrackers) {
	trackersList := make([]JsonMap, len(trackers))
	trackerStats := make([]JsonMap, len(trackers))

	for i, value := range trackers {
		trackersList[i] = make(JsonMap)
		trackersList[i]["announce"] = value.Url
		trackersList[i]["id"] = i // TODO
		trackersList[i]["scrape"] = value.Url
		trackersList[i]["tier"] = 0 // TODO

		trackerStats[i] = make(JsonMap)
		for key, value := range transmission.TrackerStatsTemplate {
			trackerStats[i][key] = value
		}
		trackerStats[i]["announce"] = value.Url
		trackerStats[i]["id"] = i // TODO
		trackerStats[i]["scrape"] = ""
		trackerStats[i]["tier"] = 0 // TODO
	}

	dst["trackers"] = trackersList
	dst["trackerStats"] = trackerStats
}

func MapPropsFiles(dst JsonMap, filesInfo []qBT.PropertiesFiles) {
	fileNum := len(filesInfo)
	files := make([]JsonMap, fileNum)
	fileStats := make([]JsonMap, fileNum)
	priorities := make([]int, fileNum)
	wanted := make([]int, fileNum)
	for i, value := range filesInfo {
		files[i] = make(JsonMap)
		fileStats[i] = make(JsonMap)

		files[i]["bytesCompleted"] = float64(value.Size) * value.Progress
		files[i]["length"] = value.Size
		files[i]["name"] = value.Name

		fileStats[i]["bytesCompleted"] = float64(value.Size) * value.Progress
		if value.Priority == 0 {
			fileStats[i]["wanted"] = false
			wanted[i] = 0
		} else {
			fileStats[i]["wanted"] = true
			wanted[i] = 1
		}
		fileStats[i]["priority"] = 0 // TODO
		priorities[i] = 0            // TODO
	}

	dst["files"] = files
	dst["fileStats"] = fileStats
	dst["priorities"] = priorities
	dst["wanted"] = wanted
}

func TorrentGet(args json.RawMessage) (JsonMap, string) {
	var req transmission.GetRequest
	err := json.Unmarshal(args, &req)
	Check(err)

	torrentList := qBTConn.GetTorrentList()

	if qBTConn.GetHashNum() == 0 {
		qBTConn.FillIDs(torrentList)
		log.Debug("Filling IDs table, new size: ", qBTConn.GetHashNum())
	}

	ids := parseIDsArgument(req.Ids)
	fields := req.Fields

	resultList := make([]JsonMap, len(ids))
	for i, id := range ids {
		translated := make(JsonMap)
		propGeneral := qBTConn.GetPropsGeneral(id)
		trackers := qBTConn.GetPropsTrackers(id)
		files := qBTConn.GetPropsFiles(id)

		MapTorrentList(translated, torrentList, id)
		MapPropsGeneral(translated, propGeneral)
		MapPropsTrackers(translated, trackers)
		MapPropsFiles(translated, files)

		translated["id"] = id
		for _, field := range fields {
			if _, ok := translated[field]; !ok {
				if !IsFieldDeprecated(field) {
					log.Error("Unsupported field: ", field)
				}
			}
		}
		for key := range translated {
			if !Any(fields, key) {
				// Remove unneeded fields
				delete(translated, key)
			}
		}
		resultList[i] = translated
	}
	return JsonMap{"torrents": resultList}, "success"
}

func SessionGet() (JsonMap, string) {
	session := make(JsonMap)
	for key, value := range transmission.SessionGetBase {
		session[key] = value
	}

	prefs := qBTConn.GetPreferences()
	session["download-dir"] = prefs.Save_path

	version := qBTConn.GetVersion()
	session["version"] = "2.84 (really qBT " + string(version) + ")"
	return session, "success"
}

func FreeSpace(args json.RawMessage) (JsonMap, string) {
	var req JsonMap
	err := json.Unmarshal(args, &req)
	Check(err)

	path := req["path"]

	return JsonMap{
		"path":       path,
		"size-bytes": float64(100 * (1 << 30)), // 100 GB
	}, "success"
}

func SessionStats() (JsonMap, string) {
	session := make(JsonMap)
	for key, value := range transmission.SessionStatsTemplate {
		session[key] = value
	}

	info := qBTConn.GetTransferInfo()
	session["downloadSpeed"] = info.Dl_info_speed
	session["uploadSpeed"] = info.Up_info_speed
	session["current-stats"].(map[string]int64)["downloadedBytes"] = info.Dl_info_data
	session["current-stats"].(map[string]int64)["uploadedBytes"] = info.Up_info_data
	session["cumulative-stats"] = session["current-stats"]
	return session, "success"
}

func TorrentPause(args json.RawMessage) (JsonMap, string) {
	hashes := parseActionArgument(args)
	for _, hash := range hashes {
		log.Debug("Stopping torrent with hash ", hash)

		qBTConn.PostForm(qBTConn.MakeRequestURL("/command/pause"),
			url.Values{"hash": {hash}})
	}
	return JsonMap{}, "success"
}

func TorrentResume(args json.RawMessage) (JsonMap, string) {
	hashes := parseActionArgument(args)
	for _, hash := range hashes {
		log.Debug("Starting torrent with hash ", hash)

		qBTConn.PostForm(qBTConn.MakeRequestURL("/command/resume"),
			url.Values{"hash": {hash}})
	}
	return JsonMap{}, "success"
}

func TorrentRecheck(args json.RawMessage) (JsonMap, string) {
	hashes := parseActionArgument(args)
	for _, hash := range hashes {
		log.Debug("Verifying torrent with hash ", hash)

		qBTConn.PostForm(qBTConn.MakeRequestURL("/command/recheck"),
			url.Values{"hash": {hash}})
	}
	return JsonMap{}, "success"
}

func TorrentDelete(args json.RawMessage) (JsonMap, string) {
	var req struct {
		Ids               json.RawMessage
		Delete_local_data interface{} `json:"delete-local-data"`
	}
	err := json.Unmarshal(args, &req)
	Check(err)

	ids := parseIDsArgument(&req.Ids)
	hashes := make([]string, len(ids))
	for i, value := range ids {
		hashes[i] = qBTConn.GetHashForId(value)
		qBTConn.HashIds[value-1] = ""
	}

	joinedHashes := strings.Join(hashes, "|")

	var deleteFiles bool // TODO: Move to a function
	switch val := req.Delete_local_data.(type) {
	case bool:
		deleteFiles = val
	case float64:
		deleteFiles = (val != 0)
	}

	if deleteFiles {
		log.Debug("Remove with files ", joinedHashes)
		qBTConn.PostForm(qBTConn.MakeRequestURL("/command/deletePerm"),
			url.Values{"hashes": {joinedHashes}})
	} else {
		log.Debug("Remove ", joinedHashes)
		qBTConn.PostForm(qBTConn.MakeRequestURL("/command/delete"),
			url.Values{"hashes": {joinedHashes}})
	}

	return JsonMap{}, "success"
}

func UploadTorrent(metainfo *[]byte, urls *string, destDir *string) {
	var buffer bytes.Buffer
	mime := multipart.NewWriter(&buffer)

	if metainfo != nil {
		mimeWriter, err := mime.CreateFormFile("torrents", "example.torrent")
		Check(err)
		mimeWriter.Write(*metainfo)
	}

	if urls != nil {
		urlsWriter, err := mime.CreateFormField("urls")
		Check(err)
		urlsWriter.Write([]byte(*urls))
	}

	if destDir != nil {
		destDirWriter, err := mime.CreateFormField("savepath")
		Check(err)
		destDirWriter.Write([]byte(*destDir))
	}
	mime.CreateFormField("cookie")
	mime.CreateFormField("label")

	mime.Close()

	qBTConn.DoPOST(qBTConn.MakeRequestURL("/command/upload"), mime.FormDataContentType(), &buffer)
	log.Debug("Torrent uploaded")
}

func ParseMagnetLink(link string) (newHash, newName string) {
	path := strings.TrimPrefix(link, "magnet:?")
	params, err := url.ParseQuery(path)
	Check(err)
	log.WithFields(log.Fields{
		"params": params,
	}).Debug("Params decoded")
	trimmed := strings.TrimPrefix(params["xt"][0], "urn:btih:")
	newHash = strings.ToLower(trimmed)
	name, nameProvided := params["dn"]
	if nameProvided {
		newName = name[0]
	} else {
		newName = "Torrent name"
	}
	return
}

func ParseMetainfo(metainfo []byte) (newHash, newName string) {
	var parsedMetaInfo MetaInfo
	parsedMetaInfo.ReadTorrentMetaInfoFile(bytes.NewBuffer(metainfo))

	log.WithFields(log.Fields{
		"len":  len(metainfo),
		"sha1": fmt.Sprintf("%x\n", sha1.Sum(metainfo)),
	}).Debug("Decoded metainfo")

	newHash = fmt.Sprintf("%x", parsedMetaInfo.InfoHash)
	newName = parsedMetaInfo.Info.Name
	return
}

func GetIdOfNewHash(newHashes []string, newHash string) (int, error) {
	for _, hash := range newHashes {
		if hash == newHash {
			return qBTConn.GetIdOfHash(newHash)
		}
	}
	return -1, errors.New("Hash not found")
}

func TorrentAdd(args json.RawMessage) (JsonMap, string) {
	var req transmission.TorrentAddRequest
	err := json.Unmarshal(args, &req)
	Check(err)

	torrentList := qBTConn.GetTorrentList()
	qBTConn.FillIDs(torrentList)

	var newHash string
	var newName string

	if req.Metainfo != nil {
		log.Debug("Upload torrent using metainfo")

		metainfo, err := base64.StdEncoding.DecodeString(*req.Metainfo)
		Check(err)
		newHash, newName = ParseMetainfo(metainfo)
		UploadTorrent(&metainfo, nil, req.Download_dir)
	} else if req.Filename != nil {
		path := *req.Filename
		if strings.HasPrefix(path, "magnet:?") {
			newHash, newName = ParseMagnetLink(path)

			if req.Download_dir != nil {
				qBTConn.PostForm(qBTConn.MakeRequestURL("/command/download"),
					url.Values{"urls": {*req.Filename}, "savepath": {*req.Download_dir}})
			} else {
				qBTConn.PostForm(qBTConn.MakeRequestURL("/command/download"),
					url.Values{"urls": {*req.Filename}})
			}
		} else if strings.HasPrefix(path, "http") {
			metainfo := DoGetWithCookies(path, req.Cookies)

			newHash, newName = ParseMetainfo(metainfo)
			UploadTorrent(&metainfo, nil, nil)
		}
	}

	log.WithFields(log.Fields{
		"hash": newHash,
		"name": newName,
	}).Debug("Attempting to add torrent")

	if newId, err := qBTConn.GetIdOfHash(newHash); err == nil {
		return JsonMap{
			"torrent-duplicate": JsonMap{
				"id":         newId,
				"name":       newName,
				"hashString": newHash,
			},
		}, "success"
	}

	newId := -1
	for retries := 0; retries < 100; retries++ {
		time.Sleep(50 * time.Millisecond)

		torrentList := qBTConn.GetTorrentList()
		newHashes := qBTConn.FillIDs(torrentList)
		if len(newHashes) > 0 {
			log.WithFields(log.Fields{
				"hashes": newHashes,
			}).Debug("New hashes")
		}

		newId, err = GetIdOfNewHash(newHashes, newHash)
		if err == nil {
			log.Debug("Found ID ", newId)
			break
		}

		log.Debug("Nothing was found, waiting...")
	}

	if newId == -1 {
		return JsonMap{}, "Torrent-add timeout"
	}

	paused := false
	if req.Paused != nil {
		if value, ok := (*req.Paused).(float64); ok {
			// Workaround: Transmission Remote GUI uses a number instead of a boolean
			paused = value != 0
		}
		if value, ok := (*req.Paused).(bool); ok {
			paused = value
		}
	}
	if paused {
		qBTConn.PostForm(qBTConn.MakeRequestURL("/command/pause"),
			url.Values{"hash": {newHash}})
	} else {
		qBTConn.PostForm(qBTConn.MakeRequestURL("/command/resume"),
			url.Values{"hash": {newHash}})
	}

	log.WithFields(log.Fields{
		"hash": newHash,
		"id":   newId,
		"name": newName,
	}).Debug("New torrent")

	return JsonMap{
		"torrent-added": JsonMap{
			"id":         newId,
			"name":       newName,
			"hashString": newHash,
		},
	}, "success"
}

func TorrentSet(args json.RawMessage) (JsonMap, string) {
	var req struct {
		Ids            *json.RawMessage
		Files_wanted   *[]int `json:"files-wanted"`
		Files_unwanted *[]int `json:"files-unwanted"`
	}
	err := json.Unmarshal(args, &req)
	Check(err)

	if req.Files_wanted != nil || req.Files_unwanted != nil {
		ids := parseIDsField(req.Ids)
		if len(ids) != 1 {
			log.Error("Unsupported torrent-set request")
			return JsonMap{}, "Unsupported torrent-set request"
		}
		id := ids[0]

		newFilesPriorities := make(map[int]int)
		if req.Files_wanted != nil {
			wanted := *req.Files_wanted
			for _, fileId := range wanted {
				newFilesPriorities[fileId] = 1 // Normal priority
			}
		}
		if req.Files_unwanted != nil {
			unwanted := *req.Files_unwanted
			for _, fileId := range unwanted {
				newFilesPriorities[fileId] = 0 // Do not download
			}
		}
		log.WithFields(log.Fields{
			"priorities": newFilesPriorities,
		}).Debug("New files priorities")

		for fileId, priority := range newFilesPriorities {
			params := url.Values{
				"hash":     {qBTConn.GetHashForId(id)},
				"id":       {strconv.Itoa(fileId)},
				"priority": {strconv.Itoa(priority)},
			}
			qBTConn.PostForm(qBTConn.MakeRequestURL("/command/setFilePrio"), params)
		}
	}

	return JsonMap{}, "success" // TODO
}

func Login(username, password string) bool {
	loginOK := qBTConn.Login(username, password)
	if loginOK {
		qBTConn.Auth.Username = username
		qBTConn.Auth.Password = password
		qBTConn.Auth.LoggedIn = true
		return true
	} else {
		return false
	}
}

func handler(w http.ResponseWriter, r *http.Request) {
	var req transmission.RPCRequest
	reqBody, err := ioutil.ReadAll(r.Body)
	log.Debug("Got request ", string(reqBody))
	err = json.Unmarshal(reqBody, &req)
	Check(err)

	if qBTConn.Auth.Required {
		username, password, ok := r.BasicAuth()
		if !ok {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		var authOK = false
		if !qBTConn.Auth.LoggedIn {
			authOK = Login(username, password)
		} else {
			authOK = (qBTConn.Auth.Username == username) && (qBTConn.Auth.Password == password)
		}
		if !authOK {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
	}

	var resp JsonMap
	var result string
	switch req.Method {
	case "session-get":
		resp, result = SessionGet()
	case "free-space":
		resp, result = FreeSpace(req.Arguments)
	case "torrent-get":
		resp, result = TorrentGet(req.Arguments)
	case "session-stats":
		resp, result = SessionStats()
	case "torrent-stop":
		resp, result = TorrentPause(req.Arguments)
	case "torrent-start":
		resp, result = TorrentResume(req.Arguments)
	case "torrent-start-now":
		resp, result = TorrentResume(req.Arguments)
	case "torrent-verify":
		resp, result = TorrentRecheck(req.Arguments)
	case "torrent-remove":
		resp, result = TorrentDelete(req.Arguments)
	case "torrent-add":
		resp, result = TorrentAdd(req.Arguments)
	case "torrent-set":
		resp, result = TorrentSet(req.Arguments)
	default:
		log.Error("Unknown method: ", req.Method)
	}
	response := JsonMap{
		"result":    result,
		"arguments": resp,
	}
	if req.Tag != nil {
		response["tag"] = req.Tag
	}
	respBody, err := json.Marshal(response)
	Check(err)
	w.Header().Set("Content-Length", strconv.Itoa(len(respBody)))
	w.Header().Set("Content-Type", "application/json; charset=UTF-8")
	w.Write(respBody)
}

func main() {
	kingpin.Parse()
	if *verbose {
		log.SetLevel(log.DebugLevel)
	} else {
		log.SetLevel(log.InfoLevel)
	}
	qBTConn.Addr = *apiAddr
	qBTConn.Tr = &http.Transport{
		DisableKeepAlives: true,
	}
	qBTConn.Client = &http.Client{Transport: qBTConn.Tr}

	qBTConn.CheckAuth()

	http.HandleFunc("/transmission/rpc", handler)
	http.HandleFunc("/rpc", handler)
	http.Handle("/", http.FileServer(http.Dir("web/")))
	err := http.ListenAndServe(":9091", nil)
	Check(err)
}
