import re, sys, glob, os
D = sys.argv[1]
src = {}
for f in glob.glob(os.path.join(D, '*.idl')):
    t = open(f, encoding='utf-8', errors='replace').read()
    t = re.sub(r'/\*.*?\*/', '', t, flags=re.S)
    t = re.sub(r'//[^\n]*', '', t)
    t = re.sub(r'cpp_quote\(".*?"\)', '', t)
    src[f] = t

ifaces = {}  # name -> (base, [methods], uuid)
pat = re.compile(r'(\[[^\]]*\]\s*)?interface\s+(\w+)\s*:\s*(\w+)\s*\{', re.S)
for f, t in src.items():
    for m in pat.finditer(t):
        attrs, name, base = m.group(1) or '', m.group(2), m.group(3)
        # brace matching
        i = m.end(); depth = 1
        while depth:
            if t[i] == '{': depth += 1
            elif t[i] == '}': depth -= 1
            i += 1
        body = t[m.end():i-1]
        # strip attribute brackets
        b = re.sub(r'\[[^\[\]]*\]', '', body)
        b = re.sub(r'\[[^\[\]]*\]', '', b)
        methods = []
        for stmt in b.split(';'):
            stmt = ' '.join(stmt.split())
            mm = re.match(r'^(?:typedef\b.*|)$', stmt)
            if not stmt or stmt.startswith('typedef') or stmt.startswith('enum') or stmt.startswith('struct'):
                continue
            mm = re.search(r'(\w+)\s*\(', stmt)
            if mm: methods.append(mm.group(1))
        # uuid: search attribute block immediately preceding (may be before 'interface' with [object, uuid(...)])
        pre = t[max(0, m.start()-600):m.start()+len(attrs)]
        us = re.findall(r'uuid\s*\(\s*([0-9a-fA-F-]+)\s*\)', (attrs or pre))
        uuid = us[-1] if us else None
        if name not in ifaces or len(methods) > len(ifaces[name][1]):
            ifaces[name] = (base, methods, uuid)
ifaces['IUnknown'] = (None, ['QueryInterface', 'AddRef', 'Release'], '00000000-0000-0000-C000-000000000046')

def chain(n):
    base, methods, _ = ifaces[n]
    return (chain(base) if base else []) + methods

want = {
 'IDXGIFactory1': ['EnumAdapters1'],
 'IDXGIAdapter1': ['EnumOutputs'],
 'IDXGIOutput1': ['GetDesc', 'DuplicateOutput'],
 'IDXGIOutputDuplication': ['GetDesc', 'AcquireNextFrame', 'ReleaseFrame'],
 'ID3D11Device': ['CreateTexture2D'],
 'ID3D11DeviceContext': ['CopyResource'],
 'ID3D11VideoDevice': ['CreateVideoProcessorEnumerator', 'CreateVideoProcessor', 'CreateVideoProcessorInputView', 'CreateVideoProcessorOutputView'],
 'ID3D11VideoContext': ['VideoProcessorSetOutputColorSpace', 'VideoProcessorSetStreamFrameFormat', 'VideoProcessorSetStreamColorSpace', 'VideoProcessorBlt'],
 'ID3D10Multithread': ['SetMultithreadProtected'],
 'IMFAttributes': ['GetUINT32', 'GetBlobSize', 'GetBlob', 'SetUINT32', 'SetUINT64', 'SetGUID'],
 'IMFActivate': ['ActivateObject', 'ShutdownObject'],
 'IMFTransform': ['GetOutputStreamInfo', 'GetAttributes', 'GetInputAvailableType', 'GetOutputAvailableType', 'SetInputType', 'SetOutputType', 'GetOutputCurrentType', 'ProcessMessage', 'ProcessInput', 'ProcessOutput'],
 'IMFMediaEventGenerator': ['GetEvent'],
 'IMFMediaEvent': ['GetType'],
 'IMFSample': ['SetSampleTime', 'SetSampleDuration', 'ConvertToContiguousBuffer', 'AddBuffer'],
 'IMFMediaBuffer': ['Lock', 'Unlock', 'SetCurrentLength'],
 'IMF2DBuffer': ['GetContiguousLength'],
 'IMFDXGIDeviceManager': ['ResetDevice'],
 'ICodecAPI': ['SetValue'],
}
print('// Code generated from mingw-w64 IDL files by genvtbl.py. DO NOT EDIT.\n')
print('//go:build windows && (amd64 || arm64)\n\npackage encoder\n\n// COM vtable slot indices (IUnknown methods included).\nconst (')
for iface, ms in want.items():
    c = chain(iface)
    for m in ms:
        if m not in c: sys.exit(f'missing {iface}.{m}')
        print(f'\tv{iface}_{m} = {c.index(m)}')
print(')\n')
print('// Interface IDs.\nvar (')
for iface in want:
    u = ifaces[iface][2]
    print(f'\tIID_{iface} = mustGUID("{{{u}}}")')
print('\tIID_ID3D11Texture2D = mustGUID("{%s}")' % ifaces['ID3D11Texture2D'][2])
print(')')
