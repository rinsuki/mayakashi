import tempfile
import os
import subprocess
import time
import glob

def make_test_source(srcdir: str):
    files = {
        "test.txt": "Hello",
        "test.for.delete.txt": "Hello",
        "test.for.overwrite.txt": "Hello",
    }

    for filename, content in files.items():
        with open(os.path.join(srcdir, filename), 'w') as f:
            f.write(content)

def run_test(mountdir: str, overlaydir: str | None):
    print("Test 1 -  アーカイブからのファイル読み込み")
    with open(os.path.join(mountdir, 'test.txt'), 'r') as f:
        assert f.read() == 'Hello'
    if overlaydir is not None:
        assert os.path.exists(os.path.join(overlaydir, 'test.txt')) == False

    print("Test 2 - 新規ファイル作成")
    with open(os.path.join(mountdir, 'test2.txt'), 'w') as f:
        f.write('Hi')
    if overlaydir is not None:
        print("Test 2.1 - オーバーレイ内に存在する")
        assert os.path.exists(os.path.join(overlaydir, 'test2.txt'))
        print("Test 2.2 - オーバーレイ内から読める")
        with open(os.path.join(overlaydir, 'test2.txt'), 'r') as f:
            assert f.read() == 'Hi'
    print("Test 2.3 - マウントポイントからの新規ファイル読み込み")
    with open(os.path.join(mountdir, 'test2.txt'), 'r') as f:
        assert f.read() == 'Hi'

    print("Test 3 - アーカイブ内ファイルの削除 (whiteout)")
    assert os.path.exists(os.path.join(mountdir, 'test.for.delete.txt'))
    os.remove(os.path.join(mountdir, 'test.for.delete.txt'))
    assert os.path.exists(os.path.join(mountdir, 'test.for.delete.txt')) == False
    
    print("Test 4 - アーカイブ内ファイルの上書き")
    with open(os.path.join(mountdir, 'test.for.overwrite.txt'), 'w') as f:
        f.write('Hi')
    print("Test 4.1 - マウントポイントからの読み込み")
    with open(os.path.join(mountdir, 'test.for.overwrite.txt'), 'r') as f:
        assert f.read() == 'Hi'
    print("Test 4.2 - マウントポイントからの削除")
    os.remove(os.path.join(mountdir, 'test.for.overwrite.txt'))
    print("Test 4.3 - マウントポイントに存在しなくなった")
    assert os.path.exists(os.path.join(mountdir, 'test.for.overwrite.txt')) == False
    print("Test 4.4 - マウントポイント内に再作成できる")
    with open(os.path.join(mountdir, 'test.for.overwrite.txt'), 'w') as f:
        f.write('Hi2')
    print("Test 4.5 - マウントポイント内に再作成したファイルを読み込める")
    with open(os.path.join(mountdir, 'test.for.overwrite.txt'), 'r') as f:
        assert f.read() == 'Hi2'
    if overlaydir is not None:
        print("Test 4.5.1 - There is no whiteout anymore")
        assert os.path.exists(os.path.join(overlaydir, 'test.for.overwrite.txt.__whiteout__')) == False

    print("Test 5 - アーカイブ内ファイルへの追記")
    with open(os.path.join(mountdir, 'test.txt'), 'a') as f:
        f.write(' World')
    print("Test 5.1 - マウントポイントから読み取れることを確認")
    with open(os.path.join(mountdir, 'test.txt'), 'r') as f:
        assert f.read() == 'Hello World'

    print("Test Done!")

def main():
    with tempfile.TemporaryDirectory() as tmpdir:
        srcdir = os.path.join(tmpdir, 'src')
        os.mkdir(srcdir)

        make_test_source(srcdir)

        mountdir = os.path.join(tmpdir, 'mount')
        # on Windows we shouldn't create mountdir before mounting
        # but on *nix we need to create it before mounting
        if os.name != 'nt':
            os.mkdir(mountdir)
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
            run_test(mountdir, overlaydir)
        finally:
            print("--- START DEBUG INFO ---")
            print("files:")
            print("\n".join(glob.glob(os.path.join(tmpdir, '**'), recursive=True)))
            print("--- END DEBUG INFO ---")
            print("Terminate mounter")
            mounter.terminate()
            mounter.wait()

if __name__ == "__main__":
    main()
