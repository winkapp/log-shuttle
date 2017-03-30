// Receiver provides mechanisms to read log requests,
// extract measurements from log requests, aggregate
// measurements in buckets, and flush buckets into a memory store.
package receiver

import (
    "sync"
    "sync/atomic"
    "time"

    "github.com/winkapp/log-shuttle/l2met/bucket"
    "github.com/winkapp/log-shuttle/l2met/metchan"
    "github.com/winkapp/log-shuttle/l2met/parser"
    "github.com/winkapp/log-shuttle/l2met/store"
    "github.com/op/go-logging"
)

var logger = logging.MustGetLogger("log-shuttle")

// We read the body of an http request and then close the request.
// The processing of the body happens in a seperate routine. We use
// this struct to hold the data that is passed inbetween routines.
type LogRequest struct {
    // The body of the HTTP request.
    Body []byte
    // Options from the query parameters
    Opts map[string][]string
}

// The register accumulates buckets in memory.
// A seperate routine working on an interval will flush
// the buckets from the register.
type register struct {
    sync.Mutex
    m map[bucket.Id]*bucket.Bucket
}

type Receiver struct {
    // Keeping a register allows us to aggregate buckets in memory.
    // This decouples redis writes from HTTP requests.
    Register *register
    // After we pull data from the HTTP requests,
    // We put the data in the inbox to be processed.
    Inbox           chan *LogRequest
    // The interval at which things are moved fron the inbox to the outbox
    TransferTicker  *time.Ticker
    // After we flush our register of buckets, we put the
    // buckets in this channel to be flushed to redis.
    Outbox          chan *bucket.Bucket
    // Flush buckets from register to redis. Number of seconds.
    FlushInterval   time.Duration
    // How many outlet routines should be running.
    NumOutlets      int
    // Bucket storage.
    Store           store.Store
    //Count the number of times we accept a bucket.
    numBuckets, numReqs uint64
    // The number of time units allowed to pass before dropping a
    // log line.
    deadline        int64
    // Publish receiver metrics on this channel.
    Mchan           *metchan.Channel
    inFlight        sync.WaitGroup
}

func NewReceiver(buffsize int, flushInt time.Duration, ccu int, rcvrd int64, s store.Store, m *metchan.Channel) *Receiver {
    r := new(Receiver)
    r.Inbox = make(chan *LogRequest, buffsize)
    r.Outbox = make(chan *bucket.Bucket, buffsize)
    r.Register = &register{m: make(map[bucket.Id]*bucket.Bucket)}
    r.FlushInterval = flushInt
    r.NumOutlets = ccu
    r.deadline = rcvrd
    r.numBuckets = uint64(0)
    r.numReqs = uint64(0)
    r.Store = s
    r.Mchan = m
    return r
}

func (r *Receiver) Receive(b []byte, opts map[string][]string) {
    logger.Debugf("Received: body: %q - opts: %+v", string(b), opts)
    r.inFlight.Add(1)
    r.Inbox <- &LogRequest{b, opts}
}

// Start moving data through the receiver's pipeline.
func (r *Receiver) Start() {
    // Accepting the data involves parsing logs messages
    // into buckets. It is mostly CPU bound, so
    // it makes sense to parallelize this to the extent
    // of the number of CPUs.
    for i := 0; i < r.NumOutlets; i++ {
        go r.accept()
    }
    // Outletting data to the store involves sending
    // data out on the network to Redis. We may wish to
    // add more threads here since it is likely that
    // they will be blocking on I/O.
    for i := 0; i < r.NumOutlets; i++ {
        go r.outlet()
    }
    r.TransferTicker = time.NewTicker(r.FlushInterval)
    // The transfer is not a concurrent process.
    // It removes buckets from the register to the outbox.
    go r.scheduleTransfer()
    go r.Report()
}

// This function can be used as
// and indicator of when it is safe
// to shutdown the process.
func (r *Receiver) Wait() {
    r.inFlight.Wait()
}

func (r *Receiver) accept() {
    for req := range r.Inbox {
        //rdr := bufio.NewReader(bytes.NewReader(req.Body))
        //TODO(DataDog): Use a cached store time.
        // The code to use here should look something like this:
        // storeTime := r.Store.Now()
        // However, since we are in a tight loop here,
        // we cant make this call. Benchmarks show that using a local
        // redis and making the time call on the redis store will slow
        // down the receive loop by 10x.
        // However, we run the risk of accepting data that is past
        // its deadline due to clock drift on the localhost. Although
        // we don't run the risk of re-reporting an interval to Librato
        // because our outlet uses the store time to process buckets.
        // So even if we write a bucket to redis that is past the
        // deadline, our outlet scanner should not pick it up because
        // it uses redis time to find buckets to process.
        storeTime := time.Now()
        startParse := time.Now()

        buckets := make([]*bucket.Bucket, 0)
        for b := range parser.BuildBuckets(req.Body, req.Opts, r.Mchan) {
            buckets = append(buckets, b)
            if b.Id.Delay(storeTime) <= r.deadline {
                r.inFlight.Add(1)
                r.addRegister(b)
            } else {
                r.Mchan.Count("receiver.drop", 1)
            }
        }
        r.Mchan.Time("receiver.accept", startParse)
        r.inFlight.Done()
    }
}

func (r *Receiver) addRegister(b *bucket.Bucket) {
    r.Register.Lock()
    defer r.Register.Unlock()
    atomic.AddUint64(&r.numBuckets, 1)
    k := *b.Id
    _, present := r.Register.m[k]
    if !present {
        r.Mchan.Count("receiver.add-bucket", 1)
        r.Register.m[k] = b
    } else {
        r.Mchan.Count("receiver.merge-bucket", 1)
        r.Register.m[k].Merge(b)
    }
}

func (r *Receiver) scheduleTransfer() {
    for range r.TransferTicker.C {
        r.transfer()
    }
}

func (r *Receiver) transfer() {
    r.Register.Lock()
    defer r.Register.Unlock()
    for k := range r.Register.m {
        if m, ok := r.Register.m[k]; ok {
            delete(r.Register.m, k)
            r.Outbox <- m
        }
    }
}

func (r *Receiver) outlet() {
    for b := range r.Outbox {
        startPut := time.Now()
        //logger.Debugf("Putting bucket in store: %v", b)
        //logger.Debugf("    Vals:        %v", b.Vals)
        //logger.Debugf("    Sum:         %f", b.Sum)
        //logger.Debugf("    Time:        %v", b.Id.Time)
        //logger.Debugf("    Resolution:  %v", b.Id.Resolution)
        //logger.Debugf("    Auth:        %s", logging.Redact(b.Id.Auth))
        //logger.Debugf("    ReadyAt:     %v", b.Id.ReadyAt)
        //logger.Debugf("    Name:        %s", b.Id.Name)
        //logger.Debugf("    Units:       %s", b.Id.Units)
        //logger.Debugf("    Source:      %s", b.Id.Source)
        //logger.Debugf("    Type:        %s", b.Id.Type)
        //logger.Debugf("    Tags:        %s", b.Id.Tags)
        if err := r.Store.Put(b); err != nil {
            logger.Errorf("error=%s", err)
        }
        r.Mchan.Time("receiver.outlet", startPut)
        r.inFlight.Done()
    }
}

//func (r *Receiver) ServeHTTP(w http.ResponseWriter, req *http.Request) {
//    atomic.AddUint64(&r.numReqs, 1)
//    defer r.Mchan.Time("http.accept", time.Now())
//    if req.Method != "POST" {
//        if !r.quiet {
//            log.Printf("error=%q\n", "Non post method received.")
//        }
//        http.Error(w, "Invalid Request", 400)
//        return
//    }
//    // If we can decrypt the authentication
//    // we know it is valid and thus good enough
//    // for our receiver. Later, another routine
//    // can extract the username and password from
//    // the auth to use it against the Librato API.
//    authLine, ok := req.Header["Authorization"]
//    if !ok && len(authLine) > 0 {
//        if !r.quiet {
//            log.Printf("error=%q\n", "Missing authorization header.")
//        }
//        http.Error(w, "Missing Auth.", 400)
//        return
//    }
//    parseRes, err := auth.Parse(authLine[0])
//    if err != nil {
//        if !r.quiet {
//            log.Printf("error=%s\n", err)
//        }
//        http.Error(w, "Fail: Parse auth.", 400)
//        return
//    }
//    var creds string
//    if creds, err = auth.Decrypt(parseRes); err != nil {
//        if !r.quiet {
//            log.Printf("error=%s\n", err)
//        }
//        http.Error(w, "Invalid Request", 400)
//        return
//    }
//    defer r.Mchan.CountReq(strings.Split(creds, ":")[0])
//    v := req.URL.Query()
//    v.Add("auth", parseRes)
//    b, err := ioutil.ReadAll(req.Body)
//    req.Body.Close()
//    if err != nil {
//        if !r.quiet {
//            log.Printf("error=%q\n", "Unable to read request body.")
//        }
//        http.Error(w, "Invalid Request", 400)
//        return
//    }
//    r.Receive(b, v)
//}

// Keep an eye on the lenghts of our bufferes.
// If they are maxed out, something is going wrong.
func (r *Receiver) Report() {
    for range time.Tick(time.Second) {
        nb := atomic.LoadUint64(&r.numBuckets)
        nr := atomic.LoadUint64(&r.numReqs)
        atomic.AddUint64(&r.numBuckets, -nb)
        atomic.AddUint64(&r.numReqs, -nr)

        if nb > 0 {
            logger.Debugf("receiver.http.num-buckets=%d", nb)
        }
        if nr > 0 {
            logger.Debugf("receiver.http.num-reqs=%d", nr)
        }

        pre := "receiver.buffer."
        r.Mchan.Count(pre + "inbox", float64(len(r.Inbox)))
        r.Mchan.Count(pre + "outbox", float64(len(r.Outbox)))
    }
}
