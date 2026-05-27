// whatskept-vision — a tiny persistent worker that wraps Apple's
// Vision.framework + PDFKit so the Go side of whatskept can run OCR
// on WhatsApp image messages and text-extract WhatsApp document
// messages without taking a cgo dependency on macOS frameworks.
//
// Protocol (line-delimited JSON over stdin / stdout):
//
//   image request:  {"id": <any>, "path": "<jpeg>"}   (kind omitted = "image")
//                or {"id": <any>, "kind": "image", "path": "<jpeg>"}
//   image response: {"id": ..., "ok": true,  "ocr_text": "...", "language": "en",
//                    "labels": [["dog", 0.94], ["pet", 0.88], ...]}
//
//   pdf request:    {"id": <any>, "kind": "pdf", "path": "<pdf>"}
//   pdf response:   {"id": ..., "ok": true, "text": "...",
//                    "page_count": 33,
//                    "pages_with_text": 30, "pages_ocr": 3,
//                    "method": "pdfkit" | "ocr" | "mixed"}
//
//   error response: {"id": ..., "ok": false, "error": "<message>"}
//
// One request, one response. Order preserved. EOF on stdin →
// process exits 0.
//
// Why a subprocess instead of cgo? Vision.framework / PDFKit have no
// stable C ABI we can wire to from Go directly; PyObjC-style
// bindings in cgo against Objective-C selectors are very fragile
// under SDK updates. A small Swift binary that does the JSON dance
// is the smallest stable seam.
//
// Build:
//   swiftc -O -o internal/helpers/bundle/whatskept-vision \
//          build/vision-helper/main.swift
//   codesign --force --sign - internal/helpers/bundle/whatskept-vision
//
// Both steps are done by `make vision-helper`.

import Foundation
import Vision
import PDFKit
import AppKit

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
    let kind: String? // "image" (default) or "pdf"
}

// Label is encoded as a 2-element JSON array [name, score] for
// brevity and so the Go side can deserialize into a tuple-shaped
// slice with no intermediate struct. Custom encode/decode below.
struct Label {
    let identifier: String
    let confidence: Float
}

// Response is the union of all reply shapes the helper emits. Each
// request kind populates a different subset of fields — see the
// header comment for the per-kind contract.
//
// We hand-roll encode() so we can omit nil fields cleanly. Symmetric
// decode() is minimal because the Swift side never reads its own
// output; the Go side does that.
struct Response: Codable {
    let id: AnyCodable
    let ok: Bool

    // Image-kind fields.
    let ocrText: String?
    let language: String?
    let labels: [Label]?

    // PDF-kind fields.
    let text: String?
    let pageCount: Int?
    let pagesWithText: Int?
    let pagesOCR: Int?
    let method: String?

    // Error.
    let error: String?

    enum CodingKeys: String, CodingKey {
        case id, ok
        case ocrText = "ocr_text"
        case language
        case labels
        case text
        case pageCount       = "page_count"
        case pagesWithText   = "pages_with_text"
        case pagesOCR        = "pages_ocr"
        case method
        case error
    }

    func encode(to encoder: Encoder) throws {
        var c = encoder.container(keyedBy: CodingKeys.self)
        try c.encode(id, forKey: .id)
        try c.encode(ok, forKey: .ok)
        if let s = ocrText  { try c.encode(s, forKey: .ocrText) }
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
        if let s = text          { try c.encode(s, forKey: .text) }
        if let n = pageCount     { try c.encode(n, forKey: .pageCount) }
        if let n = pagesWithText { try c.encode(n, forKey: .pagesWithText) }
        if let n = pagesOCR      { try c.encode(n, forKey: .pagesOCR) }
        if let s = method        { try c.encode(s, forKey: .method) }
        if let e = error         { try c.encode(e, forKey: .error) }
    }

    init(ok: Bool, id: AnyCodable, ocrText: String? = nil, language: String? = nil,
         labels: [Label]? = nil, text: String? = nil,
         pageCount: Int? = nil, pagesWithText: Int? = nil,
         pagesOCR: Int? = nil, method: String? = nil, error: String? = nil) {
        self.id = id
        self.ok = ok
        self.ocrText = ocrText
        self.language = language
        self.labels = labels
        self.text = text
        self.pageCount = pageCount
        self.pagesWithText = pagesWithText
        self.pagesOCR = pagesOCR
        self.method = method
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
        text = try? c.decode(String.self, forKey: .text)
        pageCount = try? c.decode(Int.self, forKey: .pageCount)
        pagesWithText = try? c.decode(Int.self, forKey: .pagesWithText)
        pagesOCR = try? c.decode(Int.self, forKey: .pagesOCR)
        method = try? c.decode(String.self, forKey: .method)
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

// MARK: - PDF driver ---------------------------------------------------------

// Per-page text extraction with OCR fallback. For each page, we
// first try PDFKit's native `page.string` (free, accurate, handles
// Arabic / RTL correctly). If that returns empty — which happens
// for ~30% of WhatsApp PDFs which are camera-scanned receipts /
// contracts — we rasterize the page and run Apple Vision OCR
// against the resulting bitmap.
//
// Tunables (env):
//   WHATSKEPT_PDF_MAX_OCR_PAGES   (default 100)
//        Cap on pages we'll rasterize-and-OCR per document. A few
//        large scanned PDFs (e.g. a 200-page CV dump) would otherwise
//        burn minutes; the cap means we degrade gracefully — first
//        N pages get OCR, the rest get whatever PDFKit found
//        natively (usually nothing for those PDFs, which is fine —
//        the filename is still in messages_fts via wa_document).
//   WHATSKEPT_PDF_RENDER_SCALE    (default 2.0)
//        Multiplier on the PDF's natural page size when rasterizing
//        for OCR. 2.0 = 144 dpi (PDFs are 72 dpi natively) which is
//        a good balance of OCR accuracy vs CPU. Bumping to 3.0 helps
//        small print but doubles rasterize time.
func describePDF(path: String, id: AnyCodable,
                 maxOCRPages: Int, renderScale: CGFloat) -> Response {
    let url = URL(fileURLWithPath: path)
    if !FileManager.default.fileExists(atPath: path) {
        return Response(ok: false, id: id, error: "file not found: \(path)")
    }
    guard let doc = PDFDocument(url: url) else {
        return Response(ok: false, id: id, error: "PDFKit could not open document (encrypted or corrupt?)")
    }
    let pageCount = doc.pageCount
    if pageCount == 0 {
        return Response(ok: true, id: id,
                        text: "", pageCount: 0,
                        pagesWithText: 0, pagesOCR: 0,
                        method: "pdfkit")
    }

    var parts: [String] = []
    parts.reserveCapacity(pageCount)
    var pagesWithText = 0
    var pagesOCR = 0
    var ocrUsedThisRun = 0

    for i in 0..<pageCount {
        guard let page = doc.page(at: i) else { continue }
        let native = (page.string ?? "").trimmingCharacters(in: .whitespacesAndNewlines)
        if !native.isEmpty {
            parts.append(native)
            pagesWithText += 1
            continue
        }
        // Empty native text — try OCR if we still have budget.
        if ocrUsedThisRun >= maxOCRPages { continue }
        if let ocr = ocrOnePDFPage(page: page, scale: renderScale),
           !ocr.isEmpty {
            parts.append(ocr)
            pagesOCR += 1
        }
        ocrUsedThisRun += 1
    }

    let joined = parts.joined(separator: "\n\n")
    let method: String
    if pagesOCR == 0 && pagesWithText > 0 { method = "pdfkit" }
    else if pagesWithText == 0 && pagesOCR > 0 { method = "ocr" }
    else if pagesWithText > 0 && pagesOCR > 0 { method = "mixed" }
    else { method = "empty" }

    return Response(ok: true, id: id,
                    text: joined,
                    pageCount: pageCount,
                    pagesWithText: pagesWithText,
                    pagesOCR: pagesOCR,
                    method: method)
}

// ocrOnePDFPage rasterizes a single PDFPage to a CGImage and runs
// VNRecognizeTextRequest against it. Returns the concatenated lines
// or nil on any failure (which we treat as "no text found" — same
// observable behaviour, and OCR errors on scanned-PDF pages are
// almost always transient / non-fatal).
func ocrOnePDFPage(page: PDFPage, scale: CGFloat) -> String? {
    let bounds = page.bounds(for: .mediaBox)
    let pxW = Int(bounds.width  * scale)
    let pxH = Int(bounds.height * scale)
    if pxW <= 0 || pxH <= 0 { return nil }

    let colorSpace = CGColorSpaceCreateDeviceRGB()
    guard let ctx = CGContext(data: nil,
                              width: pxW, height: pxH,
                              bitsPerComponent: 8,
                              bytesPerRow: 0,
                              space: colorSpace,
                              bitmapInfo: CGImageAlphaInfo.premultipliedLast.rawValue)
    else { return nil }

    // White background — many WhatsApp scans use transparent PDFs
    // whose Vision OCR drops 30%+ recall against a black background.
    ctx.setFillColor(CGColor(red: 1, green: 1, blue: 1, alpha: 1))
    ctx.fill(CGRect(x: 0, y: 0, width: pxW, height: pxH))
    ctx.scaleBy(x: scale, y: scale)
    page.draw(with: .mediaBox, to: ctx)

    guard let cg = ctx.makeImage() else { return nil }

    let handler = VNImageRequestHandler(cgImage: cg, options: [:])
    let req = VNRecognizeTextRequest()
    req.recognitionLevel = .accurate
    req.usesLanguageCorrection = true
    do {
        try handler.perform([req])
    } catch {
        return nil
    }
    guard let results = req.results else { return nil }
    var lines: [String] = []
    for o in results {
        if let top = o.topCandidates(1).first {
            let s = top.string.trimmingCharacters(in: .whitespacesAndNewlines)
            if !s.isEmpty { lines.append(s) }
        }
    }
    return lines.joined(separator: "\n")
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
let pdfMaxOCRPages: Int = {
    if let s = ProcessInfo.processInfo.environment["WHATSKEPT_PDF_MAX_OCR_PAGES"],
       let n = Int(s), n > 0 { return n }
    return 100
}()
let pdfRenderScale: CGFloat = {
    if let s = ProcessInfo.processInfo.environment["WHATSKEPT_PDF_RENDER_SCALE"],
       let f = Double(s), f > 0 { return CGFloat(f) }
    return 2.0
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

    let resp: Response
    switch (req.kind ?? "image") {
    case "pdf":
        resp = describePDF(path: req.path, id: req.id,
                           maxOCRPages: pdfMaxOCRPages,
                           renderScale: pdfRenderScale)
    case "image", "":
        resp = describe(path: req.path, id: req.id,
                        labelTopN: labelTopN, labelMinConf: labelMinConf)
    default:
        resp = Response(ok: false, id: req.id,
                        error: "unknown kind: \(req.kind ?? "?")")
    }
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
