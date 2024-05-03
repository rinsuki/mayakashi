import tempfile
import os
import subprocess
import time

with tempfile.TemporaryDirectory() as tmpdir:
    srcdir = os.path.join(tmpdir, 'src')
    os.mkdir(srcdir)

    with open(os.path.join(srcdir, 'test.txt'), 'w') as f:
        f.write('Hello')

    with open(os.path.join(srcdir, 'test.for.delete.txt'), 'w') as f:
        f.write('Hello')
    
    with open(os.path.join(srcdir, 'test.for.overwrite.txt'), 'w') as f:
        f.write('Hello')

    mountdir = os.path.join(tmpdir, 'mount')
    # os.mkdir(mountdir)
    overlaydir = os.path.join(tmpdir, 'overlay')
    os.mkdir(overlaydir)
    print("Create Archive")
    subprocess.run([
        "./mayakashi.exe",
        "create",
        "-i", srcdir,
        "-o", os.path.join(tmpdir, 'hello'),
        "-j", "2"
    ]).check_returncode()
    print("Mount Archive")
    mounter = subprocess.Popen([
        "./marmounter.exe",
        os.path.join(tmpdir, "hello.mar"),
        "overlaydir=" + overlaydir,
        "--",
        mountdir,
    ])
    try:
        # first, we need to wait until mounter is ready
        start_time = time.time()
        while time.time() - start_time < 60:
            time.sleep(1)
            if mounter.poll() is not None:
                raise Exception("mounter unexpectedly terminated with code " + str(mounter.returncode))
            if os.path.exists(os.path.join(mountdir, 'test.txt')):
                break
        
        print("Test 1 - Read from Archive")

        with open(os.path.join(mountdir, 'test.txt'), 'r') as f:
            assert f.read() == 'Hello'
        assert os.path.exists(os.path.join(overlaydir, 'test.txt')) == False

        print("Test 2 - Create new file")
        with open(os.path.join(mountdir, 'test2.txt'), 'w') as f:
            f.write('Hi')
        print("Test 2.1 - Exists on overlay directory")
        assert os.path.exists(os.path.join(overlaydir, 'test2.txt'))
        print("Test 2.2 - Readable from overlay directory")
        with open(os.path.join(overlaydir, 'test2.txt'), 'r') as f:
            assert f.read() == 'Hi'
        print("Test 2.3 - Readable from mounted directory")
        with open(os.path.join(mountdir, 'test2.txt'), 'r') as f:
            assert f.read() == 'Hi'

        print("Test 3 - Delete from archive (whiteout)")
        assert os.path.exists(os.path.join(mountdir, 'test.for.delete.txt'))
        os.remove(os.path.join(mountdir, 'test.for.delete.txt'))
        assert os.path.exists(os.path.join(mountdir, 'test.for.delete.txt')) == False
        
        print("Test 4 - Overwrite to archive file")
        with open(os.path.join(mountdir, 'test.for.overwrite.txt'), 'w') as f:
            f.write('Hi')
        print("Test 4.1 - Read from mounted directory")
        with open(os.path.join(mountdir, 'test.for.overwrite.txt'), 'r') as f:
            assert f.read() == 'Hi'

        print("Test 5 - Append to archive file")
        with open(os.path.join(mountdir, 'test.txt'), 'a') as f:
            f.write(' World')
        print("Test 5.1 - Read from mounted directory")
        with open(os.path.join(mountdir, 'test.txt'), 'r') as f:
            assert f.read() == 'Hello World'

        print("Test Done!")
    finally:
        mounter.terminate()
    