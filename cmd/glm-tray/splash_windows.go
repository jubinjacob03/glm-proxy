//go:build windows

package main

import (
	"bytes"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"io"
	"math"
	"net/http"
	"path/filepath"
	"runtime"
	"sync"
	"syscall"
	"time"
	"unsafe"
)

const (
	splashMinVisible    = 900 * time.Millisecond
	splashIndeterminate = -1
	splashFrameInterval = 8 * time.Millisecond

	splashBaseWidth  = 440
	splashBaseHeight = 232

	wmDestroy          = 0x0002
	wmPaint            = 0x000F
	wmClose            = 0x0010
	wmEraseBackground  = 0x0014
	wmTimer            = 0x0113
	wmApp              = 0x8000
	wmSplashUpdate     = wmApp + 1
	wmSplashClose      = wmApp + 2
	wsPopup            = 0x80000000
	wsExTopmost        = 0x00000008
	wsExToolWindow     = 0x00000080
	wsExNoActivate     = 0x08000000
	csHorizontalRedraw = 0x0002
	csVerticalRedraw   = 0x0001
	swShowNoActivate   = 4
	idcArrow           = 32512
	spiGetWorkArea     = 0x0030
	smCXScreen         = 0
	smCYScreen         = 1
	dibRGBColors       = 0
	biRGB              = 0
	srcCopy            = 0x00CC0020
	transparent        = 1
	dtCenter           = 0x0001
	dtVerticalCenter   = 0x0004
	dtSingleLine       = 0x0020
	dtNoPrefix         = 0x0800
	dtEndEllipsis      = 0x8000
	cleartypeQuality   = 5
	fwNormal           = 400
	fwSemiBold         = 600
)

var (
	splashUser32                  = syscall.NewLazyDLL("user32.dll")
	splashGDI32                   = syscall.NewLazyDLL("gdi32.dll")
	splashKernel32                = syscall.NewLazyDLL("kernel32.dll")
	splashWinMM                   = syscall.NewLazyDLL("winmm.dll")
	procRegisterClassExW          = splashUser32.NewProc("RegisterClassExW")
	procUnregisterClassW          = splashUser32.NewProc("UnregisterClassW")
	procCreateWindowExW           = splashUser32.NewProc("CreateWindowExW")
	procDefWindowProcW            = splashUser32.NewProc("DefWindowProcW")
	procDestroyWindow             = splashUser32.NewProc("DestroyWindow")
	procShowWindow                = splashUser32.NewProc("ShowWindow")
	procUpdateWindow              = splashUser32.NewProc("UpdateWindow")
	procGetMessageW               = splashUser32.NewProc("GetMessageW")
	procTranslateMessage          = splashUser32.NewProc("TranslateMessage")
	procDispatchMessageW          = splashUser32.NewProc("DispatchMessageW")
	procPostMessageW              = splashUser32.NewProc("PostMessageW")
	procPostThreadMessageW        = splashUser32.NewProc("PostThreadMessageW")
	procPostQuitMessage           = splashUser32.NewProc("PostQuitMessage")
	procBeginPaint                = splashUser32.NewProc("BeginPaint")
	procEndPaint                  = splashUser32.NewProc("EndPaint")
	procInvalidateRect            = splashUser32.NewProc("InvalidateRect")
	procSetTimer                  = splashUser32.NewProc("SetTimer")
	procKillTimer                 = splashUser32.NewProc("KillTimer")
	procGetDC                     = splashUser32.NewProc("GetDC")
	procReleaseDC                 = splashUser32.NewProc("ReleaseDC")
	procLoadCursorW               = splashUser32.NewProc("LoadCursorW")
	procSystemParametersInfoW     = splashUser32.NewProc("SystemParametersInfoW")
	procGetSystemMetrics          = splashUser32.NewProc("GetSystemMetrics")
	procSetWindowRgn              = splashUser32.NewProc("SetWindowRgn")
	procSetProcessDpiAwarenessCtx = splashUser32.NewProc("SetProcessDpiAwarenessContext")
	procGetDpiForSystem           = splashUser32.NewProc("GetDpiForSystem")
	procCreateCompatibleDC        = splashGDI32.NewProc("CreateCompatibleDC")
	procDeleteDC                  = splashGDI32.NewProc("DeleteDC")
	procCreateDIBSection          = splashGDI32.NewProc("CreateDIBSection")
	procSelectObject              = splashGDI32.NewProc("SelectObject")
	procDeleteObject              = splashGDI32.NewProc("DeleteObject")
	procBitBlt                    = splashGDI32.NewProc("BitBlt")
	procCreateRoundRectRgn        = splashGDI32.NewProc("CreateRoundRectRgn")
	procCreateFontW               = splashGDI32.NewProc("CreateFontW")
	procSetBkMode                 = splashGDI32.NewProc("SetBkMode")
	procSetTextColor              = splashGDI32.NewProc("SetTextColor")
	procGdiFlush                  = splashGDI32.NewProc("GdiFlush")
	procDrawTextW                 = splashUser32.NewProc("DrawTextW")
	procGetModuleHandleW          = splashKernel32.NewProc("GetModuleHandleW")
	procGetCurrentThreadID        = splashKernel32.NewProc("GetCurrentThreadId")
	procTimeBeginPeriod           = splashWinMM.NewProc("timeBeginPeriod")
	procTimeEndPeriod             = splashWinMM.NewProc("timeEndPeriod")
	splashWindowCallback          = syscall.NewCallback(splashWindowProc)
	splashWindowRegistry          sync.Map
)

type splashView struct {
	percent int
	text    string
}

type splash struct {
	mu             sync.Mutex
	view           splashView
	hwnd           uintptr
	threadID       uint32
	shownAt        time.Time
	firstPaintAt   time.Time
	frames         int
	closing        bool
	ready          chan struct{}
	done           chan struct{}
	firstPaint     chan struct{}
	readyOnce      sync.Once
	doneOnce       sync.Once
	firstPaintOnce sync.Once
	closeOnce      sync.Once
}

type splashWindow struct {
	owner                 *splash
	hwnd                  uintptr
	width                 int
	height                int
	dpi                   uint32
	memoryDC              uintptr
	bitmap                uintptr
	previousBitmap        uintptr
	titleFont             uintptr
	statusFont            uintptr
	pixels                []uint32
	basePixels            []uint32
	baseText              string
	baseReady             bool
	logo                  []uint32
	logoWidth             int
	logoHeight            int
	startedAt             time.Time
	lastFrameAt           time.Time
	shownProgress         float64
	wasIndeterminate      bool
	contentReady          bool
	timerResolution       bool
	frames                int
	resourcesDisposedOnce sync.Once
}

type splashPoint struct {
	x int32
	y int32
}

type splashRect struct {
	left   int32
	top    int32
	right  int32
	bottom int32
}

type splashMessage struct {
	hwnd    uintptr
	message uint32
	wParam  uintptr
	lParam  uintptr
	time    uint32
	point   splashPoint
	private uint32
}

type splashPaintStruct struct {
	dc        uintptr
	erase     int32
	paint     splashRect
	restore   int32
	incUpdate int32
	reserved  [32]byte
}

type splashWindowClass struct {
	size        uint32
	style       uint32
	windowProc  uintptr
	classExtra  int32
	windowExtra int32
	instance    uintptr
	icon        uintptr
	cursor      uintptr
	background  uintptr
	menuName    *uint16
	className   *uint16
	iconSmall   uintptr
}

type splashBitmapInfoHeader struct {
	size            uint32
	width           int32
	height          int32
	planes          uint16
	bitCount        uint16
	compression     uint32
	sizeImage       uint32
	xPixelsPerM     int32
	yPixelsPerM     int32
	colorsUsed      uint32
	colorsImportant uint32
}

type splashRGBQuad struct {
	blue     byte
	green    byte
	red      byte
	reserved byte
}

type splashBitmapInfo struct {
	header splashBitmapInfoHeader
	colors [1]splashRGBQuad
}

func newSplash(_ string) *splash {
	s := &splash{
		view:       splashView{percent: splashIndeterminate, text: "Checking for updates..."},
		ready:      make(chan struct{}),
		done:       make(chan struct{}),
		firstPaint: make(chan struct{}),
	}
	go s.run()
	select {
	case <-s.ready:
	case <-time.After(500 * time.Millisecond):
	}
	return s
}

func (s *splash) set(percent int, text string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	if s.closing {
		s.mu.Unlock()
		return
	}
	s.view = splashView{percent: percent, text: text}
	hwnd := s.hwnd
	s.mu.Unlock()
	if hwnd != 0 {
		procPostMessageW.Call(hwnd, wmSplashUpdate, 0, 0)
	}
}

func (s *splash) close() {
	if s == nil {
		return
	}
	s.closeOnce.Do(func() {
		s.mu.Lock()
		s.closing = true
		s.mu.Unlock()
		<-s.ready

		s.mu.Lock()
		hwnd := s.hwnd
		threadID := s.threadID
		shownAt := s.shownAt
		s.mu.Unlock()

		if !shownAt.IsZero() {
			if remaining := splashMinVisible - time.Since(shownAt); remaining > 0 {
				time.Sleep(remaining)
			}
		}
		if hwnd != 0 {
			posted, _, _ := procPostMessageW.Call(hwnd, wmSplashClose, 0, 0)
			if posted == 0 && threadID != 0 {
				procPostThreadMessageW.Call(uintptr(threadID), wmSplashClose, 0, 0)
			}
		}
		select {
		case <-s.done:
		case <-time.After(2 * time.Second):
			logUpdate("splash: native window did not close within 2s")
		}
	})
}

func (s *splash) run() {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	defer s.finish()

	threadID, _, _ := procGetCurrentThreadID.Call()
	s.mu.Lock()
	s.threadID = uint32(threadID)
	closing := s.closing
	s.mu.Unlock()
	if closing {
		s.markReady()
		return
	}
	if err := s.runWindow(); err != nil {
		logUpdate("splash: native window unavailable (%v)", err)
	}
}

func (s *splash) runWindow() error {
	setSplashDPIAwareness()
	dpi := splashDPI()
	width := splashScale(splashBaseWidth, dpi)
	height := splashScale(splashBaseHeight, dpi)
	className := fmt.Sprintf("GLMProxySplash_%d_%p", time.Now().UnixNano(), s)
	classNameUTF16, err := syscall.UTF16PtrFromString(className)
	if err != nil {
		s.markReady()
		return err
	}
	titleUTF16, _ := syscall.UTF16PtrFromString("GLM Proxy")
	instance, _, callErr := procGetModuleHandleW.Call(0)
	if instance == 0 {
		s.markReady()
		return splashAPIError("GetModuleHandleW", callErr)
	}
	cursor, _, _ := procLoadCursorW.Call(0, idcArrow)
	windowClass := splashWindowClass{
		size:       uint32(unsafe.Sizeof(splashWindowClass{})),
		style:      csHorizontalRedraw | csVerticalRedraw,
		windowProc: splashWindowCallback,
		instance:   instance,
		cursor:     cursor,
		className:  classNameUTF16,
	}
	atom, _, callErr := procRegisterClassExW.Call(uintptr(unsafe.Pointer(&windowClass)))
	if atom == 0 {
		s.markReady()
		return splashAPIError("RegisterClassExW", callErr)
	}
	defer procUnregisterClassW.Call(uintptr(unsafe.Pointer(classNameUTF16)), instance)

	workArea := splashWorkArea()
	x := int(workArea.left) + (int(workArea.right-workArea.left)-width)/2
	y := int(workArea.top) + (int(workArea.bottom-workArea.top)-height)/2
	hwnd, _, callErr := procCreateWindowExW.Call(
		wsExTopmost|wsExToolWindow|wsExNoActivate,
		uintptr(unsafe.Pointer(classNameUTF16)),
		uintptr(unsafe.Pointer(titleUTF16)),
		wsPopup,
		uintptr(x),
		uintptr(y),
		uintptr(width),
		uintptr(height),
		0,
		0,
		instance,
		0,
	)
	if hwnd == 0 {
		s.markReady()
		return splashAPIError("CreateWindowExW", callErr)
	}

	window := &splashWindow{
		owner:  s,
		hwnd:   hwnd,
		width:  width,
		height: height,
		dpi:    dpi,
	}
	if err := window.initialize(); err != nil {
		procDestroyWindow.Call(hwnd)
		s.markReady()
		return err
	}
	splashWindowRegistry.Store(hwnd, window)

	s.mu.Lock()
	s.hwnd = hwnd
	closing := s.closing
	if !closing {
		s.shownAt = time.Now()
	}
	s.mu.Unlock()

	if closing {
		procDestroyWindow.Call(hwnd)
		s.markReady()
		window.dispose()
		splashWindowRegistry.Delete(hwnd)
		return nil
	}

	cornerDiameter := splashScale(24, dpi) * 2
	region, _, _ := procCreateRoundRectRgn.Call(0, 0, uintptr(width+1), uintptr(height+1), uintptr(cornerDiameter), uintptr(cornerDiameter))
	if region != 0 {
		accepted, _, _ := procSetWindowRgn.Call(hwnd, region, 0)
		if accepted == 0 {
			procDeleteObject.Call(region)
		}
	}
	procShowWindow.Call(hwnd, swShowNoActivate)
	procUpdateWindow.Call(hwnd)
	s.markReady()
	window.initializeContent()
	procInvalidateRect.Call(hwnd, 0, 0)
	resolutionResult, _, _ := procTimeBeginPeriod.Call(1)
	window.timerResolution = resolutionResult == 0
	procSetTimer.Call(hwnd, 1, uintptr(splashFrameInterval/time.Millisecond), 0)

	var message splashMessage
	for {
		result, _, _ := procGetMessageW.Call(uintptr(unsafe.Pointer(&message)), 0, 0, 0)
		if int32(result) <= 0 {
			break
		}
		if message.hwnd == 0 && message.message == wmSplashClose {
			procDestroyWindow.Call(hwnd)
			continue
		}
		procTranslateMessage.Call(uintptr(unsafe.Pointer(&message)))
		procDispatchMessageW.Call(uintptr(unsafe.Pointer(&message)))
	}
	procDestroyWindow.Call(hwnd)
	window.dispose()
	splashWindowRegistry.Delete(hwnd)
	return nil
}

func (s *splash) snapshot() splashView {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.view
}

func (s *splash) recordFirstPaint(at time.Time) {
	s.mu.Lock()
	if s.firstPaintAt.IsZero() {
		s.firstPaintAt = at
	}
	s.mu.Unlock()
	s.firstPaintOnce.Do(func() { close(s.firstPaint) })
}

func (s *splash) markReady() {
	s.readyOnce.Do(func() { close(s.ready) })
}

func (s *splash) finish() {
	s.mu.Lock()
	s.hwnd = 0
	s.threadID = 0
	s.mu.Unlock()
	s.markReady()
	s.firstPaintOnce.Do(func() { close(s.firstPaint) })
	s.doneOnce.Do(func() { close(s.done) })
}

func splashWindowProc(hwnd uintptr, message uint32, wParam, lParam uintptr) uintptr {
	value, ok := splashWindowRegistry.Load(hwnd)
	if !ok {
		result, _, _ := procDefWindowProcW.Call(hwnd, uintptr(message), wParam, lParam)
		return result
	}
	window := value.(*splashWindow)
	switch message {
	case wmEraseBackground:
		return 1
	case wmPaint:
		window.paint()
		return 0
	case wmTimer:
		if wParam == 1 {
			procInvalidateRect.Call(hwnd, 0, 0)
		}
		return 0
	case wmSplashUpdate:
		procInvalidateRect.Call(hwnd, 0, 0)
		return 0
	case wmSplashClose, wmClose:
		procDestroyWindow.Call(hwnd)
		return 0
	case wmDestroy:
		procKillTimer.Call(hwnd, 1)
		procPostQuitMessage.Call(0)
		return 0
	}
	result, _, _ := procDefWindowProcW.Call(hwnd, uintptr(message), wParam, lParam)
	return result
}

func (w *splashWindow) initialize() error {
	dc, _, callErr := procGetDC.Call(w.hwnd)
	if dc == 0 {
		return splashAPIError("GetDC", callErr)
	}
	defer procReleaseDC.Call(w.hwnd, dc)

	memoryDC, _, callErr := procCreateCompatibleDC.Call(dc)
	if memoryDC == 0 {
		return splashAPIError("CreateCompatibleDC", callErr)
	}
	w.memoryDC = memoryDC
	bitmapInfo := splashBitmapInfo{header: splashBitmapInfoHeader{
		size:        uint32(unsafe.Sizeof(splashBitmapInfoHeader{})),
		width:       int32(w.width),
		height:      -int32(w.height),
		planes:      1,
		bitCount:    32,
		compression: biRGB,
	}}
	var bits unsafe.Pointer
	bitmap, _, callErr := procCreateDIBSection.Call(
		dc,
		uintptr(unsafe.Pointer(&bitmapInfo)),
		dibRGBColors,
		uintptr(unsafe.Pointer(&bits)),
		0,
		0,
	)
	if bitmap == 0 || bits == nil {
		w.dispose()
		return splashAPIError("CreateDIBSection", callErr)
	}
	w.bitmap = bitmap
	previousBitmap, _, _ := procSelectObject.Call(memoryDC, bitmap)
	w.previousBitmap = previousBitmap
	w.pixels = unsafe.Slice((*uint32)(bits), w.width*w.height)
	w.startedAt = time.Now()
	w.lastFrameAt = w.startedAt
	w.wasIndeterminate = true
	return nil
}

func (w *splashWindow) initializeContent() {
	fontName, _ := syscall.UTF16PtrFromString("Segoe UI")
	w.titleFont = createSplashFont(splashScale(20, w.dpi), fwSemiBold, fontName)
	w.statusFont = createSplashFont(splashScale(13, w.dpi), fwNormal, fontName)
	w.logoWidth = splashScale(72, w.dpi)
	w.logoHeight = w.logoWidth
	w.logo = decodeSplashLogo(trayIcon, w.logoWidth, w.logoHeight, splashPixel(0x15, 0x17, 0x1A))
	w.basePixels = make([]uint32, len(w.pixels))
	w.contentReady = true
}

func (w *splashWindow) dispose() {
	w.resourcesDisposedOnce.Do(func() {
		w.owner.mu.Lock()
		w.owner.frames = w.frames
		w.owner.mu.Unlock()
		if w.timerResolution {
			procTimeEndPeriod.Call(1)
			w.timerResolution = false
		}
		if w.memoryDC != 0 && w.previousBitmap != 0 {
			procSelectObject.Call(w.memoryDC, w.previousBitmap)
		}
		if w.titleFont != 0 {
			procDeleteObject.Call(w.titleFont)
		}
		if w.statusFont != 0 {
			procDeleteObject.Call(w.statusFont)
		}
		if w.bitmap != 0 {
			procDeleteObject.Call(w.bitmap)
		}
		if w.memoryDC != 0 {
			procDeleteDC.Call(w.memoryDC)
		}
		w.pixels = nil
		w.basePixels = nil
	})
}

func (w *splashWindow) paint() {
	var paint splashPaintStruct
	dc, _, _ := procBeginPaint.Call(w.hwnd, uintptr(unsafe.Pointer(&paint)))
	if dc == 0 {
		return
	}
	defer procEndPaint.Call(w.hwnd, uintptr(unsafe.Pointer(&paint)))
	w.frames++

	procGdiFlush.Call()
	background := splashPixel(0x15, 0x17, 0x1A)
	if !w.contentReady {
		for index := range w.pixels {
			w.pixels[index] = background
		}
		procBitBlt.Call(dc, 0, 0, uintptr(w.width), uintptr(w.height), w.memoryDC, 0, 0, srcCopy)
		w.owner.recordFirstPaint(time.Now())
		return
	}

	now := time.Now()
	view := w.owner.snapshot()
	if !w.baseReady || view.text != w.baseText {
		for index := range w.pixels {
			w.pixels[index] = background
		}
		w.drawLogo()
		w.drawText("GLM Proxy", w.titleFont, splashScale(112, w.dpi), splashScale(146, w.dpi), splashColorRef(255, 255, 255))
		w.drawText(view.text, w.statusFont, splashScale(147, w.dpi), splashScale(179, w.dpi), splashColorRef(0x9B, 0xA6, 0xAE))
		procGdiFlush.Call()
		copy(w.basePixels, w.pixels)
		w.baseText = view.text
		w.baseReady = true
	} else {
		copy(w.pixels, w.basePixels)
	}
	elapsed := now.Sub(w.lastFrameAt)
	if elapsed < time.Millisecond {
		elapsed = time.Millisecond
	}
	if elapsed > 100*time.Millisecond {
		elapsed = 100 * time.Millisecond
	}
	w.lastFrameAt = now
	indeterminate := view.percent < 0
	if !indeterminate {
		if w.wasIndeterminate {
			w.shownProgress = 0
		}
		w.shownProgress = easeSplashProgress(w.shownProgress, float64(clampSplashPercent(view.percent)), elapsed)
	}
	w.wasIndeterminate = indeterminate

	barX := splashScale(70, w.dpi)
	barY := splashScale(192, w.dpi)
	barWidth := w.width - 2*barX
	barHeight := splashScale(6, w.dpi)
	drawSplashCapsule(w.pixels, w.width, w.height, float64(barX), float64(barY), float64(barWidth), float64(barHeight), splashPixel(0x23, 0x27, 0x2C), 1)
	if indeterminate {
		trailWidth := float64(barWidth) * 2 / 3
		trailStart := splashMarqueeStart(now.Sub(w.startedAt), float64(barWidth), trailWidth)
		drawSplashTrail(w.pixels, w.width, w.height, float64(barX), float64(barY), float64(barWidth), float64(barHeight), trailStart, trailWidth)
	} else if w.shownProgress > 0.2 {
		fillWidth := float64(barWidth) * w.shownProgress / 100
		if fillWidth < float64(barHeight) {
			fillWidth = float64(barHeight)
		}
		drawSplashFill(w.pixels, w.width, w.height, float64(barX), float64(barY), fillWidth, float64(barHeight))
	}

	procBitBlt.Call(dc, 0, 0, uintptr(w.width), uintptr(w.height), w.memoryDC, 0, 0, srcCopy)
	w.owner.recordFirstPaint(time.Now())
}

func (w *splashWindow) drawLogo() {
	if len(w.logo) == 0 {
		return
	}
	x := (w.width - w.logoWidth) / 2
	y := splashScale(32, w.dpi)
	for row := 0; row < w.logoHeight && y+row < w.height; row++ {
		destination := (y+row)*w.width + x
		source := row * w.logoWidth
		copy(w.pixels[destination:destination+w.logoWidth], w.logo[source:source+w.logoWidth])
	}
}

func (w *splashWindow) drawText(text string, font uintptr, top, bottom int, textColor uintptr) {
	if text == "" || font == 0 {
		return
	}
	utf16, err := syscall.UTF16FromString(text)
	if err != nil || len(utf16) <= 1 {
		return
	}
	previous, _, _ := procSelectObject.Call(w.memoryDC, font)
	procSetBkMode.Call(w.memoryDC, transparent)
	procSetTextColor.Call(w.memoryDC, textColor)
	rect := splashRect{left: int32(splashScale(12, w.dpi)), top: int32(top), right: int32(w.width - splashScale(12, w.dpi)), bottom: int32(bottom)}
	procDrawTextW.Call(
		w.memoryDC,
		uintptr(unsafe.Pointer(&utf16[0])),
		uintptr(len(utf16)-1),
		uintptr(unsafe.Pointer(&rect)),
		dtCenter|dtVerticalCenter|dtSingleLine|dtNoPrefix|dtEndEllipsis,
	)
	procSelectObject.Call(w.memoryDC, previous)
}

func setSplashDPIAwareness() {
	if procSetProcessDpiAwarenessCtx.Find() == nil {
		procSetProcessDpiAwarenessCtx.Call(^uintptr(3))
	}
}

func splashDPI() uint32 {
	if procGetDpiForSystem.Find() == nil {
		value, _, _ := procGetDpiForSystem.Call()
		if value >= 96 && value <= 480 {
			return uint32(value)
		}
	}
	return 96
}

func splashScale(value int, dpi uint32) int {
	return (value*int(dpi) + 48) / 96
}

func splashWorkArea() splashRect {
	var workArea splashRect
	ok, _, _ := procSystemParametersInfoW.Call(spiGetWorkArea, 0, uintptr(unsafe.Pointer(&workArea)), 0)
	if ok != 0 && workArea.right > workArea.left && workArea.bottom > workArea.top {
		return workArea
	}
	width, _, _ := procGetSystemMetrics.Call(smCXScreen)
	height, _, _ := procGetSystemMetrics.Call(smCYScreen)
	return splashRect{right: int32(width), bottom: int32(height)}
}

func createSplashFont(height int, weight uintptr, name *uint16) uintptr {
	font, _, _ := procCreateFontW.Call(
		uintptr(uint32(int32(-height))),
		0,
		0,
		0,
		weight,
		0,
		0,
		0,
		1,
		0,
		0,
		cleartypeQuality,
		0,
		uintptr(unsafe.Pointer(name)),
	)
	return font
}

func splashAPIError(operation string, err error) error {
	if errno, ok := err.(syscall.Errno); !ok || errno == 0 {
		return fmt.Errorf("%s failed", operation)
	}
	return fmt.Errorf("%s: %w", operation, err)
}

func clampSplashPercent(percent int) int {
	if percent < 0 {
		return 0
	}
	if percent > 100 {
		return 100
	}
	return percent
}

func easeSplashProgress(current, target float64, elapsed time.Duration) float64 {
	factor := 1 - math.Exp(-float64(elapsed)/float64(130*time.Millisecond))
	result := current + (target-current)*factor
	if math.Abs(target-result) < 0.05 {
		return target
	}
	return result
}

func splashMarqueeStart(elapsed time.Duration, barWidth, trailWidth float64) float64 {
	phase := math.Mod(float64(elapsed), float64(1400*time.Millisecond)) / float64(1400*time.Millisecond)
	eased := 0.0
	if phase < 0.5 {
		eased = 2 * phase * phase
	} else {
		eased = 1 - math.Pow(-2*phase+2, 2)/2
	}
	return eased*(barWidth+trailWidth) - trailWidth
}

func splashPixel(red, green, blue uint8) uint32 {
	return uint32(red)<<16 | uint32(green)<<8 | uint32(blue)
}

func splashColorRef(red, green, blue uint8) uintptr {
	return uintptr(red) | uintptr(green)<<8 | uintptr(blue)<<16
}

func blendSplashPixel(destination, source uint32, opacity float64) uint32 {
	if opacity <= 0 {
		return destination
	}
	if opacity >= 1 {
		return source
	}
	destinationRed := float64((destination >> 16) & 0xFF)
	destinationGreen := float64((destination >> 8) & 0xFF)
	destinationBlue := float64(destination & 0xFF)
	sourceRed := float64((source >> 16) & 0xFF)
	sourceGreen := float64((source >> 8) & 0xFF)
	sourceBlue := float64(source & 0xFF)
	red := uint8(destinationRed + (sourceRed-destinationRed)*opacity + 0.5)
	green := uint8(destinationGreen + (sourceGreen-destinationGreen)*opacity + 0.5)
	blue := uint8(destinationBlue + (sourceBlue-destinationBlue)*opacity + 0.5)
	return splashPixel(red, green, blue)
}

func splashCapsuleCoverage(pixelX, pixelY, x, y, width, height float64) float64 {
	if width <= 0 || height <= 0 {
		return 0
	}
	radius := math.Min(height/2, width/2)
	centerY := y + height/2
	leftCenter := x + radius
	rightCenter := x + width - radius
	nearestX := math.Max(leftCenter, math.Min(rightCenter, pixelX))
	distance := math.Hypot(pixelX-nearestX, pixelY-centerY)
	return math.Max(0, math.Min(1, radius+0.5-distance))
}

func drawSplashCapsule(pixels []uint32, stride, canvasHeight int, x, y, width, height float64, pixel uint32, opacity float64) {
	left := max(0, int(math.Floor(x)))
	top := max(0, int(math.Floor(y)))
	right := min(stride, int(math.Ceil(x+width)))
	bottom := min(canvasHeight, int(math.Ceil(y+height)))
	for py := top; py < bottom; py++ {
		for px := left; px < right; px++ {
			coverage := splashCapsuleCoverage(float64(px)+0.5, float64(py)+0.5, x, y, width, height)
			index := py*stride + px
			pixels[index] = blendSplashPixel(pixels[index], pixel, coverage*opacity)
		}
	}
}

func drawSplashFill(pixels []uint32, stride, canvasHeight int, x, y, width, height float64) {
	left := max(0, int(math.Floor(x)))
	top := max(0, int(math.Floor(y)))
	right := min(stride, int(math.Ceil(x+width)))
	bottom := min(canvasHeight, int(math.Ceil(y+height)))
	for py := top; py < bottom; py++ {
		for px := left; px < right; px++ {
			coverage := splashCapsuleCoverage(float64(px)+0.5, float64(py)+0.5, x, y, width, height)
			position := (float64(px) + 0.5 - x) / width
			pixel := splashSheen(splashCyanGradient(position), (float64(py)+0.5-y)/height)
			index := py*stride + px
			pixels[index] = blendSplashPixel(pixels[index], pixel, coverage)
		}
	}
}

func drawSplashTrail(pixels []uint32, stride, canvasHeight int, barX, barY, barWidth, barHeight, trailStart, trailWidth float64) {
	left := max(0, int(math.Floor(barX)))
	top := max(0, int(math.Floor(barY)))
	right := min(stride, int(math.Ceil(barX+barWidth)))
	bottom := min(canvasHeight, int(math.Ceil(barY+barHeight)))
	absoluteTrailStart := barX + trailStart
	for py := top; py < bottom; py++ {
		for px := left; px < right; px++ {
			pixelX := float64(px) + 0.5
			pixelY := float64(py) + 0.5
			barCoverage := splashCapsuleCoverage(pixelX, pixelY, barX, barY, barWidth, barHeight)
			trailCoverage := splashCapsuleCoverage(pixelX, pixelY, absoluteTrailStart, barY, trailWidth, barHeight)
			coverage := math.Min(barCoverage, trailCoverage)
			if coverage <= 0 {
				continue
			}
			position := (pixelX - absoluteTrailStart) / trailWidth
			pixel, opacity := splashTrailGradient(position)
			pixel = splashSheen(pixel, (pixelY-barY)/barHeight)
			index := py*stride + px
			pixels[index] = blendSplashPixel(pixels[index], pixel, coverage*opacity)
		}
	}
}

func splashCyanGradient(position float64) uint32 {
	position = math.Max(0, math.Min(1, position))
	if position < 0.65 {
		return interpolateSplashColor(splashPixel(0x0E, 0x9B, 0xB8), splashPixel(0x22, 0xD3, 0xEE), position/0.65)
	}
	return interpolateSplashColor(splashPixel(0x22, 0xD3, 0xEE), splashPixel(0xB8, 0xF5, 0xFE), (position-0.65)/0.35)
}

func splashTrailGradient(position float64) (uint32, float64) {
	position = math.Max(0, math.Min(1, position))
	switch {
	case position < 0.45:
		return splashPixel(0x0E, 0x9B, 0xB8), position / 0.45 * 0.28
	case position < 0.85:
		amount := (position - 0.45) / 0.40
		return interpolateSplashColor(splashPixel(0x0E, 0x9B, 0xB8), splashPixel(0x22, 0xD3, 0xEE), amount), 0.28 + amount*0.56
	default:
		amount := (position - 0.85) / 0.15
		return interpolateSplashColor(splashPixel(0x22, 0xD3, 0xEE), splashPixel(0xB8, 0xF5, 0xFE), amount), 0.84 + amount*0.16
	}
}

// splashSheen lifts the top of the pill toward white, the soft specular
// highlight that reads as a glossy, modern progress bar.
func splashSheen(pixel uint32, verticalPos float64) uint32 {
	highlight := 1 - 2*verticalPos
	if highlight <= 0 {
		return pixel
	}
	return blendSplashPixel(pixel, splashPixel(0xFF, 0xFF, 0xFF), highlight*0.18)
}

func interpolateSplashColor(from, to uint32, amount float64) uint32 {
	amount = math.Max(0, math.Min(1, amount))
	fromRed := float64((from >> 16) & 0xFF)
	fromGreen := float64((from >> 8) & 0xFF)
	fromBlue := float64(from & 0xFF)
	toRed := float64((to >> 16) & 0xFF)
	toGreen := float64((to >> 8) & 0xFF)
	toBlue := float64(to & 0xFF)
	return splashPixel(
		uint8(fromRed+(toRed-fromRed)*amount+0.5),
		uint8(fromGreen+(toGreen-fromGreen)*amount+0.5),
		uint8(fromBlue+(toBlue-fromBlue)*amount+0.5),
	)
}

func decodeSplashLogo(iconData []byte, width, height int, background uint32) []uint32 {
	frame, ok := largestPNGFrame(iconData)
	if !ok {
		return nil
	}
	decoded, err := png.Decode(bytes.NewReader(frame))
	if err != nil {
		return nil
	}
	return scaleSplashImage(decoded, width, height, background)
}

func scaleSplashImage(source image.Image, width, height int, background uint32) []uint32 {
	if width <= 0 || height <= 0 || source.Bounds().Dx() <= 0 || source.Bounds().Dy() <= 0 {
		return nil
	}
	bounds := source.Bounds()
	sourceWidth := bounds.Dx()
	sourceHeight := bounds.Dy()
	sourcePixels := make([]color.NRGBA, sourceWidth*sourceHeight)
	for y := 0; y < sourceHeight; y++ {
		for x := 0; x < sourceWidth; x++ {
			sourcePixels[y*sourceWidth+x] = color.NRGBAModel.Convert(source.At(bounds.Min.X+x, bounds.Min.Y+y)).(color.NRGBA)
		}
	}
	result := make([]uint32, width*height)
	for y := 0; y < height; y++ {
		sourceY := (float64(y)+0.5)*float64(sourceHeight)/float64(height) - 0.5
		y0 := max(0, min(sourceHeight-1, int(math.Floor(sourceY))))
		y1 := min(sourceHeight-1, y0+1)
		yAmount := math.Max(0, sourceY-float64(y0))
		for x := 0; x < width; x++ {
			sourceX := (float64(x)+0.5)*float64(sourceWidth)/float64(width) - 0.5
			x0 := max(0, min(sourceWidth-1, int(math.Floor(sourceX))))
			x1 := min(sourceWidth-1, x0+1)
			xAmount := math.Max(0, sourceX-float64(x0))
			top := interpolateNRGBA(sourcePixels[y0*sourceWidth+x0], sourcePixels[y0*sourceWidth+x1], xAmount)
			bottom := interpolateNRGBA(sourcePixels[y1*sourceWidth+x0], sourcePixels[y1*sourceWidth+x1], xAmount)
			pixel := interpolateNRGBA(top, bottom, yAmount)
			result[y*width+x] = blendSplashPixel(background, splashPixel(pixel.R, pixel.G, pixel.B), float64(pixel.A)/255)
		}
	}
	return result
}

func interpolateNRGBA(from, to color.NRGBA, amount float64) color.NRGBA {
	return color.NRGBA{
		R: uint8(float64(from.R) + (float64(to.R)-float64(from.R))*amount + 0.5),
		G: uint8(float64(from.G) + (float64(to.G)-float64(from.G))*amount + 0.5),
		B: uint8(float64(from.B) + (float64(to.B)-float64(from.B))*amount + 0.5),
		A: uint8(float64(from.A) + (float64(to.A)-float64(from.A))*amount + 0.5),
	}
}

func largestPNGFrame(iconData []byte) ([]byte, bool) {
	const headerSize = 6
	const entrySize = 16
	if len(iconData) < headerSize || iconData[2] != 1 || iconData[3] != 0 {
		return nil, false
	}
	count := int(iconData[4]) | int(iconData[5])<<8
	if count == 0 || count > (len(iconData)-headerSize)/entrySize {
		return nil, false
	}
	var best []byte
	bestArea := 0
	for index := 0; index < count; index++ {
		entry := iconData[headerSize+index*entrySize : headerSize+(index+1)*entrySize]
		width := int(entry[0])
		height := int(entry[1])
		if width == 0 {
			width = 256
		}
		if height == 0 {
			height = 256
		}
		size := uint64(entry[8]) | uint64(entry[9])<<8 | uint64(entry[10])<<16 | uint64(entry[11])<<24
		offset := uint64(entry[12]) | uint64(entry[13])<<8 | uint64(entry[14])<<16 | uint64(entry[15])<<24
		if size < 8 || offset > uint64(len(iconData)) || size > uint64(len(iconData))-offset {
			continue
		}
		frame := iconData[int(offset):int(offset+size)]
		if !bytes.Equal(frame[:8], []byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1A, '\n'}) {
			continue
		}
		if area := width * height; area > bestArea {
			best = frame
			bestArea = area
		}
	}
	return best, best != nil
}

func waitProxyReady(dir string, timeout time.Duration) {
	port := getEnvValue(filepath.Join(dir, envFileName), "PORT")
	if port == "" {
		port = "3007"
	}
	endpoint := "http://127.0.0.1:" + port + "/health"
	client := &http.Client{Timeout: 1500 * time.Millisecond}
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		response, err := client.Get(endpoint)
		if err == nil {
			_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 1<<10))
			response.Body.Close()
			return
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return
		}
		time.Sleep(min(250*time.Millisecond, remaining))
	}
}

func humanMB(bytes int64) string {
	return fmt.Sprintf("%.1f MB", float64(bytes)/(1<<20))
}
