import contextlib,importlib.util,io,json,os,tempfile,unittest
from unittest.mock import patch
from pathlib import Path
spec=importlib.util.spec_from_file_location('release',Path(__file__).with_name('image_release.py'))
m=importlib.util.module_from_spec(spec);spec.loader.exec_module(m)

class ReleaseTests(unittest.TestCase):
    def test_existing_tag_cannot_change(self):
        with patch.object(m,'get',return_value=(b'old',{})),patch.object(m,'request') as write:
            with self.assertRaisesRegex(RuntimeError,'immutable'):m.publish('p/a','build-1',b'new')
            write.assert_not_called()
    def test_identical_retry_is_read_only(self):
        raw=b'{"schemaVersion":2,"mediaType":"application/vnd.oci.image.index.v1+json","manifests":[]}'
        with patch.object(m,'get',return_value=(raw,{})),patch.object(m,'request') as write:
            self.assertEqual(m.publish('p/a','build-1',raw),m.digest(raw));write.assert_not_called()
    def test_missing_architecture_never_publishes(self):
        with patch.object(m,'get',side_effect=[(b'{}',{}),RuntimeError('missing arm64')]),patch.object(m,'check_platforms',return_value=[]),patch.object(m,'publish') as publish:
            with self.assertRaisesRegex(RuntimeError,'missing'):m.merge('p/a','build-1',['amd64','arm64'],['latest'])
            publish.assert_not_called()
    def test_wrong_platform_rejected(self):
        with patch.object(m,'descriptors',return_value=[{'platform':{'os':'linux','architecture':'amd64'}}]):
            with self.assertRaisesRegex(RuntimeError,'expected'):m.check_platforms('p/a',b'{}',['arm64'])
    def test_merge_preserves_attestations_and_uses_batch_tags(self):
        arch=[{'digest':'sha256:a','platform':{'os':'linux','architecture':'amd64'}},{'digest':'sha256:aa','platform':{'os':'unknown','architecture':'unknown'},'annotations':{'vnd.docker.reference.type':'attestation-manifest'}}]
        arm=[{'digest':'sha256:b','platform':{'os':'linux','architecture':'arm64'}}]
        with patch.object(m,'get',return_value=(b'{}',{})) as get,patch.object(m,'check_platforms',side_effect=[arch,arm,arch+arm]),patch.object(m,'publish',return_value='sha256:final') as publish,patch.object(m,'record'):
            m.merge('p/a','build-27',['amd64','arm64'],['latest'])
            self.assertEqual([c.args for c in get.call_args_list],[('p/a','build-27-amd64'),('p/a','build-27-arm64')])
            self.assertEqual(json.loads(publish.call_args_list[0].args[2])['manifests'],arch+arm)
            self.assertFalse(publish.call_args_list[0].kwargs.get('mutable',False))
            self.assertTrue(publish.call_args_list[1].kwargs['mutable'])
    def test_render_uses_published_digest_without_touching_other_images(self):
        before=os.getcwd()
        with tempfile.TemporaryDirectory() as temp:
            try:
                os.chdir(temp);Path('.ci/digests').mkdir(parents=True)
                Path('.ci/deploy-files.json').write_text('["deploy.yaml"]')
                ref='docker.nexus.ixuni.win/p/a:build-1@sha256:'+'f'*64
                Path('.ci/digests/p__a.ref').write_text(ref+'\n')
                Path('deploy.yaml').write_text('image: docker.nexus.ixuni.win/p/a:old@sha256:'+'a'*64+'\nother: public/image:latest\n')
                m.pin_files()
                self.assertEqual(Path('deploy.yaml').read_text(),'image: '+ref+'\nother: public/image:latest\n')
            finally:os.chdir(before)
if __name__=='__main__':unittest.main()
