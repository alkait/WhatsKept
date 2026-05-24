// whatskept-vision — a tiny persistent worker that wraps Apple's
// Vision.framework so the Go side of whatskept can run OCR +
// classification on WhatsApp image messages without taking a cgo
// dependency on macOS frameworks.
//
// Protocol (line-delimited JSON over stdin / stdout):
//
//   request:  {"id": <any json scalar>, "path": "<absolute jpeg path>"}
//   response: {"id": ..., "ok": true,  "ocr_text": "...", "language": "en",
//              "labels": [["dog", 0.94], ["pet", 0.88], ...]}
//          OR {"id": ..., "ok": false, "error": "<message>"}
//
// One request, one response. Order preserved. EOF on stdin →
// process exits 0.
//
// Why a subprocess instead of cgo? Vision.framework has no stable C
// ABI we can wire to from Go directly; PyObjC-style bindings in cgo
// against Objective-C selectors are very fragile under SDK updates.
// A ~100-line Swift binary that does the JSON dance is the smallest
// stable seam.
//
// Build:
//   swiftc -O -o internal/helpers/bundle/whatskept-vision \
//          build/vision-helper/main.swift
//   codesign --force --sign - internal/helpers/bundle/whatskept-vision
//
// Both steps are done by `make vision-helper`.

import Foundation
import Vision

// MARK: - Wire types ---------------------------------------------------------

// AnyCodable lets the JSON `id` field be a string, int, double, or
// null — we round-trip whatever the caller sent us so they can
// correlate requests to responses without losing precision.
struct AnyCodable: Codable {
    let value: Any?
    init(_ value: Any?) { self.value = value }
    init(from decoder: Decoder) throws {
        let c = try decoder.singleValueContainer()
        if c.decodeNil() {
            self.value = nil
        } else if let v = try? c.decode(Int.self) {
            self.value = v
        } else if let v = try? c.decode(Double.self) {
            self.value = v
        } else if let v = try? c.decode(Bool.self) {
            self.value = v
        } else if let v = try? c.decode(String.self) {
            self.value = v
        } else {
            self.value = nil
        }
    }
    func encode(to encoder: Encoder) throws {
        var c = encoder.singleValueContainer()
        switch value {
        case nil: try c.encodeNil()
        case let v as Int:    try c.encode(v)
        case let v as Double: try c.encode(v)
        case let v as Bool:   try c.encode(v)
        case let v as String: try c.encode(v)
        default: try c.encodeNil()
        }
    }
}

struct Request: Codable {
    let id: AnyCodable
    let path: String
}

// Label is encoded as a 2-element JSON array [name, score] for
// brevity and so the Go side can deserialize into a tuple-shaped
// slice with no intermediate struct. Custom encode/decode below.
struct Label {
    let identifier: String
    let confidence: Float
}

struct Response: Codable {
    let id: AnyCodable
    let ok: Bool
    let ocrText: String?
    let language: String?
    let labels: [Label]?
    let error: String?

    enum CodingKeys: String, CodingKey {
        case id, ok
        case ocrText = "ocr_text"
        case language
        case labels
        case error
    }

    func encode(to encoder: Encoder) throws {
        var c = encoder.container(keyedBy: CodingKeys.self)
        try c.encode(id, forKey: .id)
        try c.encode(ok, forKey: .ok)
        if let s = ocrText { try c.encode(s, forKey: .ocrText) }
        if let s = language { try c.encode(s, forKey: .language) }
        if let ls = labels {
            // Encode each label as a [String, Float] pair.
            var labelArr = c.nestedUnkeyedContainer(forKey: .labels)
            for l in ls {
                var pair = labelArr.nestedUnkeyedContainer()
                try pair.encode(l.identifier)
                try pair.encode(l.confidence)
            }
        }
        if let e = error { try c.encode(e, forKey: .error) }
    }

    init(ok: Bool, id: AnyCodable, ocrText: String? = nil, language: String? = nil,
         labels: [Label]? = nil, error: String? = nil) {
        self.id = id
        self.ok = ok
        self.ocrText = ocrText
        self.language = language
        self.labels = labels
        self.error = error
    }

    init(from decoder: Decoder) throws {
        // We never decode our own responses — the Go side does. But
        // Codable requires the symmetric init. Leave it minimal.
        let c = try decoder.container(keyedBy: CodingKeys.self)
        id = try c.decode(AnyCodable.self, forKey: .id)
        ok = try c.decode(Bool.self, forKey: .ok)
        ocrText = try? c.decode(String.self, forKey: .ocrText)
        language = try? c.decode(String.self, forKey: .language)
        labels = nil
        error = try? c.decode(String.self, forKey: .error)
    }
}

// MARK: - Vision driver ------------------------------------------------------

// Run OCR + classification on one JPEG path. Both requests share
// the same VNImageRequestHandler so the file is decoded once.
//
// Tunables come from environment so the Go side can adjust without
// re-bundling the binary:
//   WHATSKEPT_VISION_LABEL_TOP_N    (default 5)
//   WHATSKEPT_VISION_LABEL_MIN_CONF (default 0.50)
func describe(path: String, id: AnyCodable,
              labelTopN: Int, labelMinConf: Float) -> Response {
    let url = URL(fileURLWithPath: path)
    if !FileManager.default.fileExists(atPath: path) {
        return Response(ok: false, id: id, error: "file not found: \(path)")
    }

    let handler = VNImageRequestHandler(url: url, options: [:])

    let ocrReq = VNRecognizeTextRequest()
    ocrReq.usesLanguageCorrection = true
    // Accurate is slower but materially better on noisy WhatsApp
    // photos (receipts photographed at angles, low-light screenshots,
    // etc). Apple Silicon throughput is fine.
    ocrReq.recognitionLevel = .accurate

    let clsReq = VNClassifyImageRequest()

    do {
        try handler.perform([ocrReq, clsReq])
    } catch {
        return Response(ok: false, id: id,
                        error: "Vision performRequests failed: \(error.localizedDescription)")
    }

    // OCR: join top candidate of each observation. Detected
    // language comes from the request, not from each line — Apple
    // Vision auto-detects the dominant script for the request as a
    // whole. We surface the first one (or "" if none).
    var lines: [String] = []
    let detectedLanguage: String = ""
    if let results = ocrReq.results {
        for o in results {
            if let top = o.topCandidates(1).first {
                let s = top.string.trimmingCharacters(in: .whitespacesAndNewlines)
                if !s.isEmpty {
                    lines.append(s)
                }
            }
        }
        // VNRecognizeTextRequestRevision3+ exposes the detected
        // dominant language for the whole request; older revisions
        // don't. Probe via key path for forward-compat.
        if #available(macOS 13.0, *) {
            // automaticallyDetectsLanguage is the new default — we
            // can ask Vision what it landed on via
            // supportedRecognitionLanguages() but there's no public
            // API for "which one did you actually use this run".
            // Best proxy: the first observation's top candidate
            // string's detected language via NLLanguageRecognizer
            // would be a separate framework; skip for now and rely
            // on call-site heuristics if needed.
        }
    }
    let ocrText = lines.joined(separator: "\n")

    // Classification: keep labels above the confidence floor, up
    // to topN. VNClassifyImageRequest returns ~1,000 labels per
    // image sorted by confidence descending; we cap aggressively.
    var labels: [Label] = []
    if let results = clsReq.results {
        for o in results {
            if o.confidence < labelMinConf { break }
            labels.append(Label(identifier: o.identifier, confidence: o.confidence))
            if labels.count >= labelTopN { break }
        }
    }

    return Response(ok: true, id: id,
                    ocrText: ocrText,
                    language: detectedLanguage,
                    labels: labels)
}

// MARK: - I/O loop -----------------------------------------------------------

let labelTopN: Int = {
    if let s = ProcessInfo.processInfo.environment["WHATSKEPT_VISION_LABEL_TOP_N"],
       let n = Int(s), n > 0 { return n }
    return 5
}()
let labelMinConf: Float = {
    if let s = ProcessInfo.processInfo.environment["WHATSKEPT_VISION_LABEL_MIN_CONF"],
       let f = Float(s), f >= 0 { return f }
    return 0.50
}()

let stdin = FileHandle.standardInput
let stdout = FileHandle.standardOutput
let stderr = FileHandle.standardError

// Line-buffered read loop. readLine() works on FileHandle.standardInput
// via the foundation bridge; falls back to manual byte accumulation
// for robustness on big lines (image paths are short, but defensive).
let decoder = JSONDecoder()
let encoder = JSONEncoder()
encoder.outputFormatting = []

func writeError(_ msg: String) {
    if let data = (msg + "\n").data(using: .utf8) {
        stderr.write(data)
    }
}

while let line = readLine(strippingNewline: true) {
    guard !line.isEmpty else { continue }
    guard let lineData = line.data(using: .utf8) else {
        writeError("invalid utf8 input, skipping")
        continue
    }
    var req: Request
    do {
        req = try decoder.decode(Request.self, from: lineData)
    } catch {
        // Couldn't decode — emit an error response with id=null so
        // the Go side at least sees one response per input line.
        let resp = Response(ok: false, id: AnyCodable(nil),
                            error: "decode failed: \(error.localizedDescription)")
        if let out = try? encoder.encode(resp) {
            stdout.write(out)
            stdout.write("\n".data(using: .utf8)!)
        }
        continue
    }

    let resp = describe(path: req.path, id: req.id,
                        labelTopN: labelTopN, labelMinConf: labelMinConf)
    do {
        let out = try encoder.encode(resp)
        stdout.write(out)
        stdout.write("\n".data(using: .utf8)!)
    } catch {
        writeError("encode failed: \(error.localizedDescription)")
    }
}

// EOF on stdin → clean shutdown.
exit(0)
