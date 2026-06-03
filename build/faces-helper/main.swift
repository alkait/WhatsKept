// whatskept-faces — a one-shot, multi-core face clusterer over a folder
// of already-decrypted JPEG/PNG/HEIC images (the workspace `media/`
// tree). It does NOT touch the encrypted backup; it works purely with
// whatever image files are already on disk.
//
// Pipeline (all on-device — no network at runtime):
//
//   1. List images in <mediaDir>.
//   2. In parallel across all cores: detect faces + roll (Apple Vision),
//      drop low-quality / tiny faces, align each face to a canonical
//      112×112 upright chip, and embed it with the bundled AdaFace
//      CoreML model → a 512-dim L2-normalized identity vector.
//   3. Cluster the embeddings with Chinese Whispers over a
//      nearest-neighbour graph (the algorithm dlib uses for face
//      clustering).
//   4. Write <outDir>/clusters.json — the grid the GUI renders — plus
//      a colour crop thumbnail per face under <outDir>/crops/.
//
// Why AdaFace and not Apple's VNGenerateImageFeaturePrint? The general
// feature-print is a *scene* descriptor: it cannot separate individuals
// within a look-alike cohort (it collapses similar-looking people into
// one blob at any threshold). AdaFace is a real face-recognition
// embedding and actually distinguishes identities. The model is fetched
// on first use (see internal/app/faces.go) and its path passed as argv.
//
// Protocol: progress is line-delimited JSON on STDOUT; human logs go to
// STDERR. One terminal `{"type":"done", ...}` line ends stdout.
//
//   usage: whatskept-faces <mediaDir> <outDir> <adaface.mlpackage>
//
// Tunables (env), all optional:
//   WHATSKEPT_FACE_CONCURRENCY  parallel image workers (default: active CPUs)
//   WHATSKEPT_FACE_MIN_QUALITY  drop faces below this capture-quality (default 0.42)
//   WHATSKEPT_FACE_MIN_SIZE     drop faces smaller than this fraction of the
//                               image's shorter side (default 0.05)
//   WHATSKEPT_FACE_THRESHOLD    graph edge distance: two faces are linked when
//                               their embedding distance is below this (default 0.85)
//
// Build (done by `make faces-helper`):
//   swiftc -O -o internal/helpers/bundle/whatskept-faces build/faces-helper/main.swift
//   codesign --force --sign - internal/helpers/bundle/whatskept-faces

import Foundation
import Vision
import AppKit
import CoreImage
import CoreML

// MARK: - Env / args ----------------------------------------------------------

let env = ProcessInfo.processInfo.environment
func envInt(_ k: String, _ d: Int) -> Int { if let s = env[k], let v = Int(s) { return v }; return d }
func envFloat(_ k: String, _ d: Float) -> Float { if let s = env[k], let v = Float(s) { return v }; return d }

// Concurrency is CAPPED low (default min(cores, 6)). The landmark
// detector dispatches its own work onto the global queue and deadlocks
// if every worker thread is busy — we must leave threads free for it.
let concurrency = max(1, min(envInt("WHATSKEPT_FACE_CONCURRENCY", min(ProcessInfo.processInfo.activeProcessorCount, 6)), 8))
let minQuality  = envFloat("WHATSKEPT_FACE_MIN_QUALITY", 0.40)
let minSizeFrac = envFloat("WHATSKEPT_FACE_MIN_SIZE", 0.05)
let threshold   = envFloat("WHATSKEPT_FACE_THRESHOLD", 0.85)
let chip: CGFloat = 112   // AdaFace input edge

let stderrHandle = FileHandle.standardError
func logLine(_ s: String) { if let d = (s + "\n").data(using: .utf8) { stderrHandle.write(d) } }
func emit(_ obj: [String: Any]) {
    guard let d = try? JSONSerialization.data(withJSONObject: obj),
          var line = String(data: d, encoding: .utf8) else { return }
    line += "\n"
    FileHandle.standardOutput.write(line.data(using: .utf8)!)
}

let args = CommandLine.arguments
guard args.count >= 4 else {
    logLine("usage: whatskept-faces <mediaDir> <outDir> <adaface.mlpackage>")
    exit(2)
}
let mediaDir = args[1], outDir = args[2], modelPath = args[3]
let cropsDir = (outDir as NSString).appendingPathComponent("crops")
let fm = FileManager.default
try? fm.createDirectory(atPath: cropsDir, withIntermediateDirectories: true)

// MARK: - Load AdaFace --------------------------------------------------------

// Exit 3 is the "model missing / unloadable" signal the Go side maps to
// a download prompt; every other failure is a generic non-zero exit.
let ciContext = CIContext(options: [.useSoftwareRenderer: false])
let vnModel: VNCoreMLModel
do {
    let compiled = try MLModel.compileModel(at: URL(fileURLWithPath: modelPath))
    let cfg = MLModelConfiguration(); cfg.computeUnits = .all
    vnModel = try VNCoreMLModel(for: try MLModel(contentsOf: compiled, configuration: cfg))
    logLine("AdaFace model loaded from \(modelPath)")
} catch {
    logLine("ERROR loading face model: \(error)")
    exit(3)
}

// MARK: - List images ---------------------------------------------------------

let imageExts: Set<String> = ["jpg", "jpeg", "png", "heic", "heif", "gif"]
var files: [String] = []
if let entries = try? fm.contentsOfDirectory(atPath: mediaDir) {
    files = entries.filter { imageExts.contains(($0 as NSString).pathExtension.lowercased()) }.sorted()
}
let total = files.count
logLine("whatskept-faces: \(total) images in \(mediaDir) · \(concurrency) workers")

func writeEmptyAndExit() -> Never {
    emit(["type": "done", "images": 0, "images_with_faces": 0, "faces": 0, "clusters": 0])
    let empty: [String: Any] = ["image_count": 0, "images_with_faces": 0, "face_count": 0,
                                "cluster_count": 0, "threshold": Double(threshold), "clusters": []]
    if let d = try? JSONSerialization.data(withJSONObject: empty, options: [.prettyPrinted]) {
        try? d.write(to: URL(fileURLWithPath: (outDir as NSString).appendingPathComponent("clusters.json")))
    }
    exit(0)
}
if total == 0 { writeEmptyAndExit() }

// MARK: - Per-face record -----------------------------------------------------

final class Face {
    let file: String, stem: String, idx: Int
    let quality: Float, crop: String
    let emb: [Float]            // 512-d L2-normalized AdaFace embedding
    init(_ file: String, _ stem: String, _ idx: Int, _ q: Float, _ crop: String, _ emb: [Float]) {
        self.file = file; self.stem = stem; self.idx = idx; self.quality = q; self.crop = crop; self.emb = emb
    }
}

let lock = NSLock()
var faces: [Face] = []
var imagesWithFaces = 0
var scanned = 0

func decode(_ p: String) -> CGImage? { NSImage(contentsOfFile: p)?.cgImage(forProposedRect: nil, context: nil, hints: nil) }

// ArcFace canonical 5-point template for a 112×112 chip, in CoreImage
// (bottom-left origin) coordinates: leftEye, rightEye, nose, leftMouth,
// rightMouth. AdaFace/ArcFace models are TRAINED on faces warped to these
// exact positions, so matching them is what makes the embedding reliable.
let tpl5: [CGPoint] = [
    CGPoint(x: 38.2946, y: chip - 51.6963), CGPoint(x: 73.5318, y: chip - 51.5014),
    CGPoint(x: 56.0252, y: chip - 71.7366), CGPoint(x: 41.5493, y: chip - 92.3655),
    CGPoint(x: 70.7299, y: chip - 92.2041),
]

func centroidInImage(_ r: VNFaceLandmarkRegion2D?, _ sz: CGSize) -> CGPoint? {
    guard let r = r, r.pointCount > 0 else { return nil }
    let pts = r.pointsInImage(imageSize: sz)
    var x: CGFloat = 0, y: CGFloat = 0
    for p in pts { x += p.x; y += p.y }
    return CGPoint(x: x / CGFloat(pts.count), y: y / CGFloat(pts.count))
}
func extremeXInImage(_ r: VNFaceLandmarkRegion2D?, _ sz: CGSize, leftmost: Bool) -> CGPoint? {
    guard let r = r, r.pointCount > 0 else { return nil }
    let pts = r.pointsInImage(imageSize: sz)
    return leftmost ? pts.min(by: { $0.x < $1.x }) : pts.max(by: { $0.x < $1.x })
}

// Least-squares 2D similarity (Procrustes, no reflection) mapping src→dst.
func similarityTransform(_ src: [CGPoint], _ dst: [CGPoint]) -> CGAffineTransform? {
    let n = src.count; if n < 2 { return nil }
    var ms = CGPoint.zero, md = CGPoint.zero
    for i in 0..<n { ms.x += src[i].x; ms.y += src[i].y; md.x += dst[i].x; md.y += dst[i].y }
    ms.x /= CGFloat(n); ms.y /= CGFloat(n); md.x /= CGFloat(n); md.y /= CGFloat(n)
    var sa: CGFloat = 0, sb: CGFloat = 0, den: CGFloat = 0
    for i in 0..<n {
        let px = src[i].x - ms.x, py = src[i].y - ms.y
        let qx = dst[i].x - md.x, qy = dst[i].y - md.y
        sa += qx * px + qy * py     // → s·cosθ · Σ|p|²
        sb += qy * px - qx * py     // → s·sinθ · Σ|p|²
        den += px * px + py * py
    }
    if den < 1e-6 { return nil }
    let a = sa / den, b = sb / den
    let tx = md.x - (a * ms.x - b * ms.y)
    let ty = md.y - (b * ms.x + a * ms.y)
    return CGAffineTransform(a: a, b: b, c: -b, d: a, tx: tx, ty: ty)
}

// Aligned 112×112 RGB chip via the 5-point template. Returns nil if the
// landmarks needed for alignment aren't available (we'd rather drop a
// face than embed a mis-aligned chip, which just pollutes clusters).
func alignedChip(_ ci: CIImage, _ obs: VNFaceObservation, _ sz: CGSize) -> CGImage? {
    guard let lm = obs.landmarks else { return nil }
    guard let e1 = centroidInImage(lm.leftEye, sz) ?? centroidInImage(lm.leftPupil, sz),
          let e2 = centroidInImage(lm.rightEye, sz) ?? centroidInImage(lm.rightPupil, sz),
          let nose = centroidInImage(lm.nose, sz),
          let lips = lm.outerLips ?? lm.innerLips,
          let m1 = extremeXInImage(lips, sz, leftmost: true),
          let m2 = extremeXInImage(lips, sz, leftmost: false) else { return nil }
    // Order eyes/mouth by image-x so they match the template's left/right.
    let L = e1.x <= e2.x ? e1 : e2, R = e1.x <= e2.x ? e2 : e1
    let ML = m1.x <= m2.x ? m1 : m2, MR = m1.x <= m2.x ? m2 : m1
    guard let xf = similarityTransform([L, R, nose, ML, MR], tpl5) else { return nil }
    let a = ci.transformed(by: xf).cropped(to: CGRect(x: 0, y: 0, width: chip, height: chip))
    return ciContext.createCGImage(a, from: CGRect(x: 0, y: 0, width: chip, height: chip))
}

// Run AdaFace on a 112×112 chip → 512-d L2-normalized vector. The model
// output is Float16; read via the type-converting subscript.
func embed(_ cg: CGImage) -> [Float]? {
    let req = VNCoreMLRequest(model: vnModel)
    req.imageCropAndScaleOption = .scaleFill
    do { try VNImageRequestHandler(cgImage: cg).perform([req]) } catch { return nil }
    guard let o = req.results?.first as? VNCoreMLFeatureValueObservation,
          let m = o.featureValue.multiArrayValue else { return nil }
    var v = [Float](repeating: 0, count: m.count)
    for i in 0..<m.count { v[i] = m[i].floatValue }
    var n: Float = 0; for x in v { n += x * x }
    n = max(1e-6, n.squareRoot()); for i in 0..<v.count { v[i] /= n }
    return v
}

func jpeg(_ cg: CGImage) -> Data? {
    NSBitmapImageRep(cgImage: cg).representation(using: .jpeg, properties: [.compressionFactor: 0.85])
}

func iou(_ a: CGRect, _ b: CGRect) -> CGFloat {
    let i = a.intersection(b); if i.isNull { return 0 }
    let ia = i.width * i.height; let ua = a.width * a.height + b.width * b.height - ia
    return ua > 0 ? ia / ua : 0
}

// MARK: - Scan (parallel) -----------------------------------------------------

func scanImage(_ i: Int) {
    let file = files[i]
    let stem = (file as NSString).deletingPathExtension
    defer {
        lock.lock(); scanned += 1; let done = scanned; let nf = faces.count; lock.unlock()
        if done % 250 == 0 || done == total { emit(["type": "progress", "phase": "scan", "done": done, "total": total, "faces": nf]) }
    }
    guard let cg = decode((mediaDir as NSString).appendingPathComponent(file)) else { return }
    let W = CGFloat(cg.width), H = CGFloat(cg.height); if W < 24 || H < 24 { return }
    let short = min(W, H)

    // Landmarks (for 5-point alignment) + capture-quality (for gating).
    let sz = CGSize(width: W, height: H)
    let handler = VNImageRequestHandler(cgImage: cg, options: [:])
    let lmReq = VNDetectFaceLandmarksRequest()
    let qualReq = VNDetectFaceCaptureQualityRequest()
    do { try handler.perform([lmReq, qualReq]) } catch { return }
    let lmFaces = lmReq.results ?? []; if lmFaces.isEmpty { return }
    let qFaces = qualReq.results ?? []

    let ci = CIImage(cgImage: cg)
    var local: [Face] = []
    var keptIdx = 0
    for obs in lmFaces {
        var q: Float = 0.5; var bestIoU: CGFloat = 0
        for qf in qFaces {
            let o = iou(qf.boundingBox, obs.boundingBox)
            if o > bestIoU, let fq = qf.faceCaptureQuality { bestIoU = o; q = fq }
        }
        if q < minQuality { continue }
        let fw = obs.boundingBox.width * W, fh = obs.boundingBox.height * H
        if min(fw, fh) < short * CGFloat(minSizeFrac) { continue }

        guard let c = alignedChip(ci, obs, sz), let e = embed(c) else { continue }
        let rel = "crops/\(stem)_\(keptIdx).jpg"
        guard let jj = jpeg(c) else { continue }
        try? jj.write(to: URL(fileURLWithPath: (outDir as NSString).appendingPathComponent(rel)))
        local.append(Face(file, stem, keptIdx, q, rel, e))
        keptIdx += 1
    }
    if !local.isEmpty { lock.lock(); faces.append(contentsOf: local); imagesWithFaces += 1; lock.unlock() }
}

// Bounded async pool (width `concurrency`, kept below the core count) so
// the landmark detector's internal GCD work always has free threads —
// concurrentPerform would saturate the pool and deadlock the detector.
do {
    let pool = DispatchQueue(label: "whatskept.faces.scan", attributes: .concurrent)
    let group = DispatchGroup()
    let sem = DispatchSemaphore(value: concurrency)
    for i in 0..<total {
        sem.wait(); group.enter()
        pool.async { scanImage(i); sem.signal(); group.leave() }
    }
    group.wait()
}
emit(["type": "progress", "phase": "scan", "done": total, "total": total, "faces": faces.count])
logLine("scan done: \(faces.count) faces across \(imagesWithFaces) images")
if faces.isEmpty { writeEmptyAndExit() }

// MARK: - Pairwise graph (parallel) -------------------------------------------

let N = faces.count
@inline(__always) func dist(_ i: Int, _ j: Int) -> Float {
    var s: Float = 0; let a = faces[i].emb, b = faces[j].emb
    for k in 0..<a.count { let d = a[k] - b[k]; s += d * d }
    return s.squareRoot()
}

struct Edge { let i: Int; let j: Int; let w: Float }
var edges: [Edge] = []
do {
    let lockE = NSLock()
    DispatchQueue.concurrentPerform(iterations: N) { i in
        var localE: [Edge] = []
        for j in (i + 1)..<N { let d = dist(i, j); if d < threshold { localE.append(Edge(i: i, j: j, w: threshold - d)) } }
        if !localE.isEmpty { lockE.lock(); edges.append(contentsOf: localE); lockE.unlock() }
        if i % 500 == 0 { emit(["type": "progress", "phase": "cluster", "done": i, "total": N]) }
    }
}
emit(["type": "progress", "phase": "cluster", "done": N, "total": N])
logLine("graph: \(edges.count) edges over \(N) faces")

// MARK: - Chinese Whispers ----------------------------------------------------

// Deterministic PRNG (SplitMix64) so clustering is reproducible run-to-run.
// Chinese Whispers is sensitive to node visit order near merge boundaries;
// an unseeded RNG made the same library flip between outcomes between runs.
struct SeededRNG: RandomNumberGenerator {
    var state: UInt64
    init(seed: UInt64) { state = seed }
    mutating func next() -> UInt64 {
        state &+= 0x9E3779B97F4A7C15
        var z = state
        z = (z ^ (z >> 30)) &* 0xBF58476D1CE4E5B9
        z = (z ^ (z >> 27)) &* 0x94D049BB133111EB
        return z ^ (z >> 31)
    }
}

func chineseWhispers(_ edgeList: [Edge], _ n: Int, iters: Int) -> [Int] {
    var adj = [[(Int, Float)]](repeating: [], count: n)
    for e in edgeList { adj[e.i].append((e.j, e.w)); adj[e.j].append((e.i, e.w)) }
    var label = Array(0..<n); var rng = SeededRNG(seed: 0xC0FFEE); var order = Array(0..<n)
    for _ in 0..<iters {
        order.shuffle(using: &rng)
        for node in order {
            let nbrs = adj[node]; if nbrs.isEmpty { continue }
            var score: [Int: Float] = [:]
            for (nb, w) in nbrs { score[label[nb], default: 0] += w }
            if let best = score.max(by: { $0.value < $1.value })?.key { label[node] = best }
        }
    }
    return label
}

let labels = chineseWhispers(edges, N, iters: 25)
var byLabel: [Int: [Int]] = [:]
for (fi, l) in labels.enumerated() { byLabel[l, default: []].append(fi) }
logLine("clustering done: \(byLabel.count) clusters")

// MARK: - Write clusters.json -------------------------------------------------

var clustersJSON: [[String: Any]] = []
var outID = 0
for members in byLabel.values.sorted(by: { $0.count > $1.count }) {
    let sorted = members.sorted { faces[$0].quality > faces[$1].quality }
    let rep = faces[sorted[0]]
    let memJSON: [[String: Any]] = sorted.prefix(300).map { fi in
        let f = faces[fi]
        return ["file": f.file, "rowid": Int(f.stem) ?? -1, "crop": f.crop, "quality": Double(f.quality)]
    }
    clustersJSON.append(["id": outID, "count": members.count, "representative": rep.crop, "members": memJSON])
    outID += 1
}
let result: [String: Any] = [
    "image_count": total, "images_with_faces": imagesWithFaces, "face_count": faces.count,
    "cluster_count": byLabel.count, "threshold": Double(threshold), "clusters": clustersJSON,
]
let outPath = (outDir as NSString).appendingPathComponent("clusters.json")
if let dd = try? JSONSerialization.data(withJSONObject: result, options: []) {
    do { try dd.write(to: URL(fileURLWithPath: outPath)) }
    catch { logLine("ERROR writing clusters.json: \(error)"); exit(1) }
}
emit(["type": "done", "images": total, "images_with_faces": imagesWithFaces, "faces": faces.count, "clusters": byLabel.count])
logLine("wrote \(outPath)")
exit(0)
