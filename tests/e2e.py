import tempfile
import os
import subprocess

with tempfile.TemporaryDirectory() as tmpdir:
    srcdir = os.path.join(tmpdir, 'src')
    os.mkdir(srcdir)
    with open(os.path.join(srcdir, 'test.txt'), 'w') as f:
        f.write('Hello')
    mountdir = os.path.join(tmpdir, 'mount')
    os.mkdir(mountdir)
    overlaydir = os.path.join(tmpdir, 'overlay')
    os.mkdir(overlaydir)
    subprocess.run(["./mayakashi.exe", "create", "-i", srcdir, "-o", os.path.join(tmpdir, 'hello'), "-j", "2"]).check_returncode()
    mounter = subprocess.Popen(["./marmounter.exe", "./hello.mar", "mountpoint=" + mountdir, "overlaydir=" + overlaydir])
    try:
        with open(os.path.join(mountdir, 'test.txt'), 'r') as f:
            assert f.read() == 'Hello'
    finally:
        mounter.terminate()
    